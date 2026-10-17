package boughline

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func RequestID() string {
	b := make([]byte, 24)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type Client struct {
	Server, Repo, Token, Socket string
	HTTP, Daemon                *http.Client
	Local                       ObjectStore
	mu                          sync.Mutex
	checkout                    Checkout
}

func NewClient(server, repo, token, socket, cache, ca string) (*Client, error) {
	server = strings.TrimRight(server, "/")
	if err := ValidateAPIEndpoint(server); err != nil {
		return nil, err
	}
	if err := ValidName(repo); err != nil {
		return nil, err
	}
	if len(token) < 16 {
		return nil, fail(401, "BOUGH_TOKEN is missing or too short")
	}
	h, err := SecureHTTP(ca)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
	}, MaxIdleConnsPerHost: 32}
	c := &Client{Server: server, Repo: repo, Token: token, Socket: socket, HTTP: h, Daemon: &http.Client{Transport: transport, Timeout: 120 * time.Second}, Local: &DiskObjects{Directory: cache, Limit: 64 << 20}}
	return c, nil
}
func (c *Client) route(resource string) string {
	return "/v1/repos/" + url.PathEscape(c.Repo) + "/" + resource
}
func (c *Client) Request(ctx context.Context, method, route string, value, output any) error {
	var data []byte
	if raw, ok := value.([]byte); ok {
		data = raw
	} else if value != nil {
		data = Encode(value)
	}
	retry := method == "GET" || strings.HasSuffix(route, "/checkout") || strings.HasSuffix(route, "/blobs") || strings.HasSuffix(route, "/commits") || strings.HasSuffix(route, "/rebase") || strings.HasSuffix(route, "/merge")
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, c.Server+route, bytes.NewReader(data))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+c.Token)
		request.Header.Set("Content-Type", "application/json")
		if _, ok := value.([]byte); ok {
			request.Header.Set("Content-Type", "application/octet-stream")
		}
		response, err := c.HTTP.Do(request)
		if err != nil {
			if retry && attempt < 2 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(40+rand.IntN(100)) * time.Millisecond):
					continue
				}
			}
			return fail(503, "source-control server unavailable")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, MaxRequest+1))
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if len(body) > MaxRequest {
			return fail(502, "server response too large")
		}
		if response.StatusCode >= 300 {
			var f Fault
			if jsonDecode(body, &f) != nil {
				f.Message = "server request failed"
			}
			f.Status = response.StatusCode
			return &f
		}
		if output == nil {
			return nil
		}
		if jsonDecode(body, output) != nil {
			return fail(502, "invalid server response")
		}
		return nil
	}
	return fail(503, "server unavailable")
}
func (c *Client) Resolve(ctx context.Context, revision string) (string, error) {
	if idPattern.MatchString(revision) {
		var output struct {
			ID        string    `json:"id"`
			Changeset Changeset `json:"changeset"`
			Complete  bool      `json:"complete"`
		}
		if err := c.Request(ctx, "GET", c.route("changes/"+revision), nil, &output); err != nil {
			return "", err
		}
		return output.ID, nil
	}
	if err := ValidName(revision); err != nil {
		return "", err
	}
	var output struct {
		Target string `json:"target"`
	}
	err := c.Request(ctx, "GET", c.route("refs/"+url.PathEscape(revision)), nil, &output)
	return output.Target, err
}
func (c *Client) Checkout(ctx context.Context, revision string) (Checkout, error) {
	var out Checkout
	err := c.Request(ctx, "POST", c.route("checkout"), map[string]string{"revision": revision}, &out)
	if err == nil {
		c.mu.Lock()
		c.checkout = out
		c.mu.Unlock()
	}
	return out, err
}
func (c *Client) Object(ctx context.Context, checkout Checkout, kind, id, path string) ([]byte, error) {
	return c.objectPage(ctx, checkout, kind, id, path, "")
}
func (c *Client) objectPage(ctx context.Context, checkout Checkout, kind, id, path, page string) ([]byte, error) {
	if data, err := GetRaw(ctx, c.Local, kind, id); err == nil {
		return data, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		latest := c.checkout
		c.mu.Unlock()
		if latest.Revision == checkout.Revision && latest.Expires.After(checkout.Expires) {
			checkout = latest
		}
		if time.Until(checkout.Expires) < 5*time.Second {
			var err error
			checkout, err = c.Checkout(ctx, checkout.Revision)
			if err != nil {
				return nil, err
			}
		}
		input := ObjectRequest{Capability: checkout.Capability, Kind: kind, ID: id, Path: path, Page: page}
		request, err := http.NewRequestWithContext(ctx, "POST", "http://bough-daemon/v1/objects/get", bytes.NewReader(Encode(input)))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.Daemon.Do(request)
		if err != nil {
			return nil, fail(503, "node daemon unavailable; no upstream bypass is permitted")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, MaxBlob+(1<<20)))
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode == 401 && attempt == 0 {
			checkout, err = c.Checkout(ctx, checkout.Revision)
			if err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode != 200 {
			var f Fault
			if jsonDecode(body, &f) != nil {
				f.Message = "object request failed"
			}
			f.Status = response.StatusCode
			return nil, &f
		}
		if response.Header.Get("X-Bough-Role") != "daemon" {
			return nil, fail(502, "node socket has the wrong tier role")
		}
		raw, err := Unpack(body, id)
		if err != nil {
			return nil, err
		}
		if err = c.Local.Put(ctx, kind, id, body); err != nil {
			return nil, err
		}
		return raw, nil
	}
	return nil, fail(401, "object authorization expired")
}
func Selected(filename string, prefixes []string) bool {
	for _, p := range prefixes {
		if under(filename, p) {
			return true
		}
	}
	return false
}
func (c *Client) Manifest(ctx context.Context, checkout Checkout, prefixes []string) (map[string]File, error) {
	files := map[string]File{}
	folded := map[string]string{}
	var walk func(string, string) error
	walk = func(id, prefix string) error {
		return c.walkDirectory(ctx, checkout, id, prefix, prefixes, nil, func(name string, entry Entry) error {
			p := name
			if prefix != "" {
				p = prefix + "/" + name
			}
			relevant := Selected(p, prefixes)
			for _, want := range prefixes {
				if under(want, p) {
					relevant = true
				}
			}
			if !relevant {
				return nil
			}
			lower := strings.ToLower(p)
			if previous, ok := folded[lower]; ok && previous != p {
				return fail(409, "case-colliding checkout paths")
			}
			folded[lower] = p
			if entry.Kind == "tree" {
				if err := walk(entry.ID, p); err != nil {
					return err
				}
			} else {
				f := File{ID: entry.ID, Mode: entry.Mode, Size: entry.Size}
				if err := f.Validate(); err != nil {
					return err
				}
				files[p] = f
			}
			return nil
		})
	}
	if err := walk(checkout.Root, ""); err != nil {
		return nil, err
	}
	return files, nil
}
func (c *Client) Land(ctx context.Context, revision, ref string) (string, error) {
	for attempt := 0; attempt < 50; attempt++ {
		head, err := c.Resolve(ctx, ref)
		if err != nil {
			return "", err
		}
		request := map[string]string{"ref": ref, "expected": head, "revision": revision, "request_id": "land-" + Hash(Encode([]string{head, revision, ref}))}
		var result struct {
			ID string `json:"id"`
		}
		err = c.Request(ctx, "POST", c.route("land"), request, &result)
		if err == nil {
			return result.ID, nil
		}
		if statusOf(err) != 409 && statusOf(err) != 503 {
			return "", err
		}
		if f, ok := err.(*Fault); ok && f.Message == "merge conflict" {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(5+rand.IntN(30*(attempt+1))) * time.Millisecond):
		}
	}
	return "", fail(409, "ref remained busy; saved changeset is safe to retry")
}
