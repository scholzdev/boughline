package boughline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"time"
)

// Regional caches and daemons receive only the public key, so they cannot mint
// broader access if a job obtains a cache's configuration.
type Capability struct {
	Version  int    `json:"version"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Root     string `json:"root"`
	Subject  string `json:"subject"`
	Expires  int64  `json:"expires"`
}

func SignCapability(c Capability, private ed25519.PrivateKey) (string, error) {
	if len(private) != ed25519.PrivateKeySize {
		return "", fail(500, "invalid signing key")
	}
	raw := Encode(c)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, raw)), nil
}
func VerifyCapability(token string, public ed25519.PublicKey, now time.Time) (Capability, error) {
	var c Capability
	if len(token) > 16384 {
		return c, fail(401, "invalid object capability")
	}
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return c, fail(401, "invalid object capability")
	}
	raw, e := base64.RawURLEncoding.DecodeString(payload)
	if e != nil {
		return c, fail(401, "invalid object capability")
	}
	signature, e := base64.RawURLEncoding.DecodeString(sig)
	if e != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, raw, signature) {
		return c, fail(401, "invalid object capability")
	}
	if jsonDecode(raw, &c) != nil || c.Version != 1 || ValidID(c.Root) != nil || ValidID(c.Revision) != nil || ValidName(c.Repo) != nil {
		return c, fail(401, "invalid object capability")
	}
	if c.Expires <= now.Unix() {
		return c, fail(401, "object capability expired")
	}
	if c.Expires > now.Add(2*time.Minute).Unix() {
		return c, fail(401, "invalid object capability lifetime")
	}
	return c, nil
}

func (e *Engine) Checkout(ctx context.Context, repo, id string, p Principal, signing ed25519.PrivateKey) (Checkout, error) {
	root, err := e.Root(ctx, repo, id)
	if err != nil {
		return Checkout{}, err
	}
	if !p.Admin {
		viewKind := "access-view/v1:" + Hash(Encode(p.Rules))
		cached, err := e.Meta.Derived(ctx, repo, id, viewKind)
		if err != nil {
			return Checkout{}, err
		}
		if cached != "" {
			root = cached
		} else {
			root, err = e.accessTree(ctx, root, "", p.Rules)
			if err != nil {
				return Checkout{}, err
			}
			if err = e.Meta.SaveDerived(ctx, repo, id, viewKind, root); err != nil {
				return Checkout{}, err
			}
		}
	}
	expires := time.Now().Add(time.Minute)
	cap, err := SignCapability(Capability{Version: 1, Repo: repo, Revision: id, Root: root, Subject: p.Name, Expires: expires.Unix()}, signing)
	if err != nil {
		return Checkout{}, err
	}
	return Checkout{Repo: repo, Revision: id, Root: root, Capability: cap, Expires: expires}, nil
}
func mayReadBelow(rules []Rule, p string) bool {
	if Allowed(rules, p, false) {
		return true
	}
	for _, r := range rules {
		if r.Read && (p == "" || strings.HasPrefix(r.Prefix, p+"/")) {
			return true
		}
	}
	return false
}
func wholeSubtree(rules []Rule, p string) bool {
	if !Allowed(rules, p, false) {
		return false
	}
	for _, r := range rules {
		if !r.Read && (p == "" || strings.HasPrefix(r.Prefix, p+"/")) {
			return false
		}
	}
	return true
}
func (e *Engine) accessTree(ctx context.Context, root, prefix string, rules []Rule) (string, error) {
	if wholeSubtree(rules, prefix) {
		return root, nil
	}
	view := root
	var err error
	if !Allowed(rules, prefix, false) {
		view, err = PutJSON(ctx, e.Objects, "tree", Tree{})
		if err != nil {
			return "", err
		}
	}
	// Only policy boundaries can alter a subtree. Preserve all unrelated pages.
	names := map[string]bool{}
	for _, rule := range rules {
		rest := rule.Prefix
		if prefix != "" {
			if !strings.HasPrefix(rest, prefix+"/") {
				continue
			}
			rest = strings.TrimPrefix(rest, prefix+"/")
		}
		if rest != "" {
			name, _, _ := strings.Cut(rest, "/")
			names[name] = true
		}
	}
	updates := map[string]*Entry{}
	for name := range names {
		entry, err := LookupEntry(ctx, e.Objects, root, name)
		if err != nil {
			return "", err
		}
		if entry == nil {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "/" + name
		}
		updates[name] = nil
		if entry.Kind == "tree" && mayReadBelow(rules, p) {
			entry.ID, err = e.accessTree(ctx, entry.ID, p, rules)
			if err != nil {
				return "", err
			}
			entry.Items, entry.Count, err = treeStats(ctx, e.Objects, entry.ID)
			if err != nil {
				return "", err
			}
			if entry.Items > 0 {
				updates[name] = entry
			}
		} else if entry.Kind != "tree" && Allowed(rules, p, false) {
			updates[name] = entry
		}
	}
	return patchEntries(ctx, e.Objects, view, updates)
}
