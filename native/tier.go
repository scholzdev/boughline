package boughline

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type TierMetrics struct {
	Requests         atomic.Uint64
	CacheHits        atomic.Uint64
	Misses           atomic.Uint64
	UpstreamFailures atomic.Uint64
	Rejected         atomic.Uint64
}

func (m *TierMetrics) Snapshot() map[string]uint64 {
	return map[string]uint64{"requests": m.Requests.Load(), "cache_hits": m.CacheHits.Load(), "misses": m.Misses.Load(), "upstream_failures": m.UpstreamFailures.Load(), "rejected": m.Rejected.Load()}
}
func SecureHTTP(caFile string) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.MaxIdleConns = 128
	t.MaxIdleConnsPerHost = 64
	t.ResponseHeaderTimeout = 30 * time.Second
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return &http.Client{Transport: t, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("credential-bearing redirect refused") }}, nil
}
func ValidateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || u.Path != "" && u.Path != "/" {
		return fail(400, "invalid service origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fail(400, "service credentials require HTTPS except on loopback")
		}
	}
	return nil
}

// API gateways may have a path prefix; cache origins remain origin-only.
func ValidateAPIEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.EscapedPath(), "%") {
		return fail(400, "invalid API endpoint")
	}
	origin := *u
	origin.Path = ""
	origin.RawPath = ""
	if err = ValidateOrigin(origin.String()); err != nil {
		return err
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return fail(400, "invalid API endpoint path")
		}
	}
	return nil
}

type UpstreamObjects struct {
	Local                     ObjectStore
	URL, Secret, ExpectedRole string
	HTTP                      *http.Client
	Metrics                   *TierMetrics
}

func (s *UpstreamObjects) Get(ctx context.Context, kind, id string) ([]byte, error) {
	b, err := s.Local.Get(ctx, kind, id)
	if err == nil {
		if _, err = Unpack(b, id); err == nil {
			s.Metrics.CacheHits.Add(1)
			return b, nil
		}
	}
	s.Metrics.Misses.Add(1)
	request, err := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/objects/raw", bytes.NewReader(Encode(ObjectRequest{Kind: kind, ID: id})))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Bough-Tier-Key", s.Secret)
	caller := "daemon"
	if s.ExpectedRole == "server" {
		caller = "cache"
	}
	request.Header.Set("X-Bough-Caller", caller)
	response, err := s.HTTP.Do(request)
	if err != nil {
		s.Metrics.UpstreamFailures.Add(1)
		return nil, fail(503, "configured cache tier unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("X-Bough-Role") != s.ExpectedRole {
		s.Metrics.UpstreamFailures.Add(1)
		return nil, fail(503, "upstream tier rejected the request or has the wrong role")
	}
	b, err = io.ReadAll(io.LimitReader(response.Body, MaxBlob+(1<<20)))
	if err != nil {
		return nil, err
	}
	if _, err = Unpack(b, id); err != nil {
		return nil, err
	}
	if err = s.Local.Put(ctx, kind, id, b); err != nil {
		return nil, err
	}
	return b, nil
}
func (s *UpstreamObjects) Put(ctx context.Context, kind, id string, b []byte) error {
	return s.Local.Put(ctx, kind, id, b)
}

type ObjectService struct {
	Role           string
	Store          ObjectStore
	Public         ed25519.PublicKey
	IncomingSecret string
	Metrics        *TierMetrics
	slots          chan struct{}
}

func NewObjectService(role string, store ObjectStore, public ed25519.PublicKey, secret string) *ObjectService {
	return &ObjectService{Role: role, Store: store, Public: public, IncomingSecret: secret, Metrics: &TierMetrics{}, slots: make(chan struct{}, 32)}
}
func (s *ObjectService) Handler(w http.ResponseWriter, r *http.Request) {
	s.Metrics.Requests.Add(1)
	if r.Method != "POST" {
		writeError(w, fail(405, "POST required"))
		return
	}
	if s.Role != "daemon" && (len(s.IncomingSecret) < 32 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Bough-Tier-Key")), []byte(s.IncomingSecret)) != 1) {
		s.Metrics.Rejected.Add(1)
		writeError(w, fail(401, "tier authentication required"))
		return
	}
	expectedCaller := "daemon"
	if s.Role == "server" {
		expectedCaller = "cache"
	}
	if s.Role != "daemon" && r.Header.Get("X-Bough-Caller") != expectedCaller {
		writeError(w, fail(403, "caller is not the required cache tier"))
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.Metrics.Rejected.Add(1)
		writeError(w, fail(503, "object tier busy"))
		return
	}
	var input ObjectRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	if input.Kind != "blob" && input.Kind != "tree" {
		writeError(w, fail(400, "checkout object kind required"))
		return
	}
	if err := ValidID(input.ID); err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Path == "/v1/objects/raw" {
		if s.Role == "daemon" {
			writeError(w, fail(403, "daemon has no raw object endpoint"))
			return
		}
	} else if r.URL.Path == "/v1/objects/get" {
		cap, err := VerifyCapability(input.Capability, s.Public, time.Now())
		if err != nil {
			writeError(w, err)
			return
		}
		if err = s.authorize(r.Context(), cap, input); err != nil {
			writeError(w, err)
			return
		}
	} else {
		writeError(w, fail(404, "object RPC not found"))
		return
	}
	packed, err := s.Store.Get(r.Context(), input.Kind, input.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("X-Bough-Role", s.Role)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(200)
	_, _ = w.Write(packed)
}
func (s *ObjectService) authorize(ctx context.Context, c Capability, r ObjectRequest) error {
	if err := ValidPath(r.Path, true); err != nil {
		return err
	}
	root := c.Root
	kind := "tree"
	if r.Path != "" {
		parts := strings.Split(r.Path, "/")
		for i, name := range parts {
			entry, err := LookupEntry(ctx, s.Store, root, name)
			if err != nil {
				return err
			}
			if entry == nil {
				return fail(403, "path is outside the authorized snapshot")
			}
			root = entry.ID
			if entry.Kind != "tree" {
				if i != len(parts)-1 {
					return fail(403, "invalid snapshot path")
				}
				kind = "blob"
			}
		}
	}
	if kind == "tree" {
		var err error
		root, err = PageID(ctx, s.Store, root, r.Page)
		if err != nil {
			return err
		}
	} else if r.Page != "" {
		return fail(403, "blob requests cannot select tree pages")
	}
	if r.Kind != kind || r.ID != root {
		return fail(403, "object is outside the authorized snapshot")
	}
	return nil
}

func (s *ObjectService) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/objects/get", s.Handler)
	mux.HandleFunc("/v1/objects/raw", s.Handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "role": s.Role})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.Role != "daemon" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Bough-Tier-Key")), []byte(s.IncomingSecret)) != 1 {
			writeError(w, fail(401, "tier authentication required"))
			return
		}
		writeJSON(w, 200, s.Metrics.Snapshot())
	})
}
