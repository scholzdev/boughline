package boughline

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fail(415, "application/json required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequest)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return fail(413, "request body too large")
	}
	if jsonDecode(data, v) != nil {
		return fail(400, "invalid JSON body")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(Encode(v))
}
func writeError(w http.ResponseWriter, err error) {
	var f *Fault
	if errors.As(err, &f) {
		if f.Status == 503 || f.Status == 429 {
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, f.Status, f)
		return
	}
	log.Printf("request failed: %v", err)
	writeJSON(w, 500, &Fault{Message: "internal error; inspect server logs"})
}

type ForgeSession struct {
	Repo    string `json:"repo"`
	Subject string `json:"subject"`
	Write   bool   `json:"write"`
	Admin   bool   `json:"admin"`
	Expires int64  `json:"expires"`
}

func SignForgeSession(s ForgeSession, secret string) string {
	raw := Encode(s)
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write(raw)
	return "forge:" + base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func verifyForgeSession(token, secret string) (ForgeSession, error) {
	var s ForgeSession
	if len(secret) < 32 || len(token) > 8192 {
		return s, fail(401, "invalid Forgejo session")
	}
	raw, sig, ok := strings.Cut(strings.TrimPrefix(token, "forge:"), ".")
	if !ok {
		return s, fail(401, "invalid Forgejo session")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return s, fail(401, "invalid Forgejo session")
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return s, fail(401, "invalid Forgejo session")
	}
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write(data)
	if !hmac.Equal(signature, h.Sum(nil)) || jsonDecode(data, &s) != nil || s.Expires <= time.Now().Unix() || s.Expires > time.Now().Add(5*time.Minute).Unix() {
		return s, fail(401, "invalid or expired Forgejo session")
	}
	return s, nil
}

type bucket struct {
	tokens float64
	last   time.Time
}
type Limiter struct {
	mu          sync.Mutex
	users       map[string]bucket
	Rate, Burst float64
}

func (l *Limiter) Allow(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.users == nil {
		l.users = map[string]bucket{}
	}
	now := time.Now()
	b, ok := l.users[id]
	if !ok {
		if len(l.users) > 8192 {
			for k, v := range l.users {
				if now.Sub(v.last) > time.Minute {
					delete(l.users, k)
				}
			}
			if len(l.users) > 8192 {
				return false
			}
		}
		b = bucket{l.Burst, now}
	}
	b.tokens += now.Sub(b.last).Seconds() * l.Rate
	if b.tokens > l.Burst {
		b.tokens = l.Burst
	}
	b.last = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.users[id] = b
	return allowed
}

type API struct {
	Engine                         *Engine
	Objects                        *ObjectService
	Signing                        ed25519.PrivateKey
	AdminToken, Owner, ForgeSecret string
	Limiter                        Limiter
	slots                          chan struct{}
}

func NewAPI(e *Engine, signing ed25519.PrivateKey, admin, owner, cacheKey string) (*API, error) {
	if len(admin) < 32 || len(cacheKey) < 32 || len(signing) != ed25519.PrivateKeySize {
		return nil, fail(400, "server secrets are missing or too short")
	}
	if err := ValidText(owner, 128); err != nil {
		return nil, err
	}
	a := &API{Engine: e, Signing: signing, AdminToken: admin, Owner: owner, Limiter: Limiter{Rate: 200, Burst: 400}, slots: make(chan struct{}, 64)}
	a.Objects = NewObjectService("server", e.Objects, signing.Public().(ed25519.PublicKey), cacheKey)
	return a, nil
}
func (a *API) principal(ctx context.Context, token, repo string) (Principal, error) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.AdminToken)) == 1 {
		return Principal{Name: a.Owner, Admin: true}, nil
	}
	if strings.HasPrefix(token, "forge:") {
		session, err := verifyForgeSession(token, a.ForgeSecret)
		if err != nil {
			return Principal{}, err
		}
		if session.Repo != repo {
			return Principal{}, fail(403, "session belongs to another repository")
		}
		grant, err := a.Engine.Meta.Grant(ctx, repo, session.Subject)
		if err != nil && statusOf(err) != 404 {
			return Principal{}, err
		}
		if statusOf(err) == 404 {
			grant = Principal{Name: session.Subject, Admin: session.Admin, Rules: []Rule{{Read: true, Write: session.Write}}}
		}
		if !session.Admin {
			grant.Admin = false
		}
		if !session.Write {
			grant.Admin = false
			for i := range grant.Rules {
				grant.Rules[i].Write = false
			}
		}
		return grant, nil
	}
	if repo != "" {
		config, err := a.Engine.Meta.Repo(ctx, repo)
		if err != nil {
			return Principal{}, err
		}
		if config.ForgejoOnly {
			return Principal{}, fail(401, "use this repository's Forgejo gateway")
		}
	}
	record, err := a.Engine.Meta.Token(ctx, Hash([]byte(token)))
	if err != nil {
		return Principal{}, err
	}
	if repo != record.Repo {
		return Principal{}, fail(403, "token belongs to another repository")
	}
	return a.Engine.Meta.Grant(ctx, repo, record.Principal.Name)
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/objects/") {
		a.Objects.Handler(w, r)
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]string{"status": "ok", "role": "server"})
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		writeError(w, fail(503, "server busy"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 110*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	repo := ""
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "repos" {
		repo = parts[2]
		if err := ValidName(repo); err != nil {
			writeError(w, err)
			return
		}
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		writeError(w, fail(401, "bearer token required"))
		return
	}
	if !a.Limiter.Allow(Hash([]byte(token))) {
		writeError(w, fail(429, "rate limit exceeded"))
		return
	}
	p, err := a.principal(ctx, token, repo)
	if err != nil {
		writeError(w, err)
		return
	}
	if !a.Limiter.Allow("principal:" + repo + ":" + p.Name) {
		writeError(w, fail(429, "principal rate limit exceeded"))
		return
	}
	if !p.Admin && !mayReadBelow(p.Rules, "") {
		writeError(w, fail(403, "repository access denied"))
		return
	}
	result, status, err := a.dispatch(w, r, parts, repo, p)
	if err != nil {
		writeError(w, err)
		return
	}
	if status != 0 {
		writeJSON(w, status, result)
	}
}
func (a *API) dispatch(w http.ResponseWriter, r *http.Request, parts []string, repo string, p Principal) (any, int, error) {
	ctx, e := r.Context(), a.Engine
	if r.Method == "POST" && r.URL.Path == "/v1/repos" {
		if !p.Admin {
			return nil, 0, fail(403, "administrator required")
		}
		var c RepoConfig
		if err := readJSON(w, r, &c); err != nil {
			return nil, 0, err
		}
		if c.Owner == "" {
			c.Owner = p.Name
		}
		id, err := e.CreateRepo(ctx, c)
		return map[string]string{"genesis": id, "repo": c.ID}, 201, err
	}
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "repos" {
		return nil, 0, fail(404, "API route not found")
	}
	if r.Method == "POST" && parts[3] == "initialize" {
		if !p.Admin || !strings.HasPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "forge:") {
			return nil, 0, fail(403, "Forgejo administrator required")
		}
		var config RepoConfig
		if err := readJSON(w, r, &config); err != nil {
			return nil, 0, err
		}
		if config.ID != repo || !config.ForgejoOnly {
			return nil, 0, fail(400, "invalid Forgejo repository configuration")
		}
		if existing, err := e.Meta.Repo(ctx, repo); err == nil {
			if existing.ForgejoOnly && existing.Owner == config.Owner {
				return map[string]string{"repo": repo}, 200, nil
			}
			return nil, 0, fail(409, "repository already exists")
		} else if statusOf(err) != 404 {
			return nil, 0, err
		}
		id, err := e.CreateRepo(ctx, config)
		return map[string]string{"repo": repo, "genesis": id}, 201, err
	}
	if _, err := e.Meta.Repo(ctx, repo); err != nil {
		return nil, 0, err
	}
	resource := parts[3]
	if r.Method == "GET" {
		switch resource {
		case "info":
			c, err := e.Meta.Repo(ctx, repo)
			if err != nil {
				return nil, 0, err
			}
			head, err := e.Meta.Ref(ctx, repo, "main")
			return map[string]any{"repo": c.ID, "owner": c.Owner, "main": head, "protocol": 1}, 200, err
		case "refs":
			if len(parts) != 5 {
				return nil, 0, fail(400, "ref name required")
			}
			target, err := e.Meta.Ref(ctx, repo, parts[4])
			return map[string]string{"target": target}, 200, err
		case "changes":
			if len(parts) != 5 {
				return nil, 0, fail(400, "changeset ID required")
			}
			c, err := e.Change(ctx, repo, parts[4])
			if err != nil {
				return nil, 0, err
			}
			filtered := map[string]*File{}
			complete := true
			for filename, v := range c.Changes {
				if p.Check(filename, false) == nil {
					filtered[filename] = v
				} else {
					complete = false
				}
			}
			c.Changes = filtered
			if !complete {
				c.Message = "[restricted change]"
				c.OriginalGit = nil
			}
			return map[string]any{"id": parts[4], "changeset": c, "complete": complete}, 200, nil
		case "browse":
			id, err := e.Resolve(ctx, repo, r.URL.Query().Get("revision"))
			if err != nil {
				return nil, 0, err
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit == 0 {
				limit = 100
			}
			result, err := e.Browse(ctx, repo, id, r.URL.Query().Get("path"), r.URL.Query().Get("cursor"), limit, p)
			return result, 200, err
		case "search", "copies":
			id, err := e.Resolve(ctx, repo, r.URL.Query().Get("revision"))
			if err != nil {
				return nil, 0, err
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit == 0 {
				limit = 100
			}
			var paths []string
			if resource == "search" {
				paths, err = e.SearchPaths(ctx, repo, id, r.URL.Query().Get("q"), limit, p)
			} else {
				paths, err = e.Copies(ctx, repo, id, r.URL.Query().Get("path"), limit, p)
			}
			return map[string]any{"paths": paths}, 200, err
		case "history":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit == 0 {
				limit = 100
			}
			id, err := e.Resolve(ctx, repo, r.URL.Query().Get("revision"))
			if err != nil {
				return nil, 0, err
			}
			nodes, next, err := e.History(ctx, repo, id, r.URL.Query().Get("path"), r.URL.Query().Get("cursor"), limit, p)
			return map[string]any{"history": nodes, "next": next}, 200, err
		case "blame":
			id, err := e.Resolve(ctx, repo, r.URL.Query().Get("revision"))
			if err != nil {
				return nil, 0, err
			}
			blame, err := e.Blame(ctx, repo, id, r.URL.Query().Get("path"), p)
			return blame, 200, err
		case "reviews":
			if len(parts) != 5 {
				return nil, 0, fail(400, "changeset ID required")
			}
			if _, err := e.Change(ctx, repo, parts[4]); err != nil {
				return nil, 0, err
			}
			reviews, err := e.Meta.Reviews(ctx, repo, parts[4])
			for i := range reviews {
				reviews[i].Principal = nil
			} // Authorization attestations are private server metadata.
			return map[string]any{"reviews": reviews}, 200, err
		case "metrics":
			if !p.Admin {
				return nil, 0, fail(403, "administrator required")
			}
			return a.Objects.Metrics.Snapshot(), 200, nil
		}
	}
	if r.Method == "POST" {
		switch resource {
		case "blobs":
			if !p.Admin {
				write := false
				for _, rule := range p.Rules {
					write = write || rule.Write
				}
				if !write {
					return nil, 0, fail(403, "write permission required")
				}
			}
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBlob))
			if err != nil {
				return nil, 0, fail(413, "blob exceeds 256 MiB")
			}
			id, err := e.UploadBlob(ctx, repo, p, raw)
			return map[string]string{"id": id}, 201, err
		case "import-ref":
			var input struct {
				Ref      string `json:"ref"`
				Expected string `json:"expected"`
				Revision string `json:"revision"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			err := e.ImportRef(ctx, repo, input.Ref, input.Expected, input.Revision, p)
			return map[string]bool{"saved": err == nil}, 200, err
		case "commits":
			var input CommitRequest
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			id, err := e.Commit(ctx, repo, p, input)
			return map[string]string{"id": id}, 201, err
		case "checkout":
			var input struct {
				Revision string `json:"revision"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			id, err := e.Resolve(ctx, repo, input.Revision)
			if err != nil {
				return nil, 0, err
			}
			checkout, err := e.Checkout(ctx, repo, id, p, a.Signing)
			return checkout, 200, err
		case "diff":
			var input struct {
				Base   string `json:"base"`
				Target string `json:"target"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			changes, err := e.Diff(ctx, repo, input.Base, input.Target, p)
			if err != nil {
				return nil, 0, err
			}
			if r.URL.Query().Get("patch") == "true" {
				patches, err := e.Patches(ctx, changes)
				return map[string]any{"changes": changes, "patches": patches}, 200, err
			}
			return map[string]any{"changes": changes}, 200, nil
		case "merge-base":
			var input struct {
				Left  string `json:"left"`
				Right string `json:"right"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			bases, err := e.MergeBases(ctx, repo, input.Left, input.Right)
			return map[string]any{"bases": bases}, 200, err
		case "merge":
			var input struct {
				Left      string `json:"left"`
				Right     string `json:"right"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			id, err := e.Merge(ctx, repo, input.Left, input.Right, input.Message, input.RequestID, p)
			return map[string]string{"id": id}, 201, err
		case "rebase":
			var input struct {
				Revision  string `json:"revision"`
				Onto      string `json:"onto"`
				RequestID string `json:"request_id"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			id, err := e.Rebase(ctx, repo, input.Revision, input.Onto, input.RequestID, p)
			return map[string]string{"id": id}, 201, err
		case "land":
			var input struct {
				Ref       string `json:"ref"`
				Expected  string `json:"expected"`
				Revision  string `json:"revision"`
				RequestID string `json:"request_id"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			if input.Ref == "" {
				input.Ref = "main"
			}
			id, err := e.Land(ctx, repo, input.Ref, input.Expected, input.Revision, input.RequestID, p)
			return map[string]string{"id": id}, 200, err
		case "review":
			var input struct {
				Revision string `json:"revision"`
				Approve  bool   `json:"approve"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			err := e.Approve(ctx, repo, input.Revision, input.Approve, p)
			return map[string]bool{"saved": err == nil}, 200, err
		case "policies":
			if !p.Admin {
				return nil, 0, fail(403, "administrator required")
			}
			var input struct {
				Policies []ReviewPolicy `json:"policies"`
			}
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			if err := ValidatePolicies(input.Policies); err != nil {
				return nil, 0, err
			}
			err := e.Meta.SetPolicies(ctx, repo, input.Policies)
			return map[string]bool{"saved": err == nil}, 200, err
		case "tokens", "principals":
			if !p.Admin {
				return nil, 0, fail(403, "administrator required")
			}
			var input Principal
			if err := readJSON(w, r, &input); err != nil {
				return nil, 0, err
			}
			if err := ValidText(input.Name, 128); err != nil {
				return nil, 0, err
			}
			if err := ValidateRules(input.Rules); err != nil {
				return nil, 0, err
			}
			if err := e.Meta.SetGrant(ctx, repo, input); err != nil {
				return nil, 0, err
			}
			if resource == "principals" {
				return map[string]bool{"saved": true}, 200, nil
			}
			secret := make([]byte, 48)
			if _, err := rand.Read(secret); err != nil {
				return nil, 0, err
			}
			token := base64.RawURLEncoding.EncodeToString(secret)
			err := e.Meta.PutToken(ctx, Hash([]byte(token)), TokenRecord{Repo: repo, Principal: Principal{Name: input.Name}})
			return map[string]string{"token": token, "hash": Hash([]byte(token)), "principal": input.Name}, 201, err
		}
	}
	if r.Method == "DELETE" && resource == "tokens" && len(parts) == 5 {
		if !p.Admin {
			return nil, 0, fail(403, "administrator required")
		}
		if err := ValidID(parts[4]); err != nil {
			return nil, 0, err
		}
		record, err := e.Meta.Token(ctx, parts[4])
		if err != nil {
			return nil, 0, err
		}
		if record.Repo != repo {
			return nil, 0, fail(403, "token belongs to another repository")
		}
		err = e.Meta.RevokeToken(ctx, parts[4])
		return map[string]bool{"revoked": err == nil}, 200, err
	}
	return nil, 0, fail(404, "API route not found")
}
