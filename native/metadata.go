package boughline

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Review struct {
	Principal *Principal `json:"principal,omitempty"`
	Revision  string     `json:"revision"`
	Reviewer  string     `json:"reviewer"`
	Approve   bool       `json:"approve"`
	Time      time.Time  `json:"time"`
}
type TokenRecord struct {
	Repo      string    `json:"repo"`
	Principal Principal `json:"principal"`
}

type Metadata interface {
	ClaimBlob(context.Context, string, string, string) error
	HasBlobClaim(context.Context, string, string, string) (bool, error)
	CreateRepo(context.Context, RepoConfig, string) error
	Repo(context.Context, string) (RepoConfig, error)
	Exists(context.Context, string, string) (bool, error)
	Request(context.Context, string, string, string) (string, error)
	Publish(context.Context, string, string, string, string, map[string]string) (string, error)
	Derived(context.Context, string, string, string) (string, error)
	SaveDerived(context.Context, string, string, string, string) error
	Ref(context.Context, string, string) (string, error)
	CAS(context.Context, string, string, string, string, ...LandGuard) error
	PutToken(context.Context, string, TokenRecord) error
	Token(context.Context, string) (TokenRecord, error)
	SetGrant(context.Context, string, Principal) error
	Grant(context.Context, string, string) (Principal, error)
	RevokeToken(context.Context, string) error
	Review(context.Context, string, Review) error
	Reviews(context.Context, string, string) ([]Review, error)
	SetPolicies(context.Context, string, []ReviewPolicy) error
	ListChanges(context.Context, string, string, int) ([]string, error)
}

type SQLMetadata struct{ Pool *pgxpool.Pool }

func OpenSQL(ctx context.Context, url string) (*SQLMetadata, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 32
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &SQLMetadata{Pool: p}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}
func (s *SQLMetadata) Migrate(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS bough_repos (id TEXT PRIMARY KEY, config JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS bough_blob_claims (repo TEXT NOT NULL REFERENCES bough_repos(id), principal TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY(repo,principal,id));
CREATE TABLE IF NOT EXISTS bough_changes (repo TEXT NOT NULL REFERENCES bough_repos(id), id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(repo,id));
CREATE TABLE IF NOT EXISTS bough_requests (repo TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL, revision TEXT NOT NULL, PRIMARY KEY(repo,key), FOREIGN KEY(repo,revision) REFERENCES bough_changes(repo,id));
CREATE TABLE IF NOT EXISTS bough_derived (repo TEXT NOT NULL, revision TEXT NOT NULL, kind TEXT NOT NULL, object TEXT NOT NULL, PRIMARY KEY(repo,revision,kind), FOREIGN KEY(repo,revision) REFERENCES bough_changes(repo,id));
CREATE TABLE IF NOT EXISTS bough_refs (repo TEXT NOT NULL, name TEXT NOT NULL, target TEXT NOT NULL, PRIMARY KEY(repo,name), FOREIGN KEY(repo,target) REFERENCES bough_changes(repo,id));
CREATE TABLE IF NOT EXISTS bough_tokens (hash TEXT PRIMARY KEY, record JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS bough_grants (repo TEXT NOT NULL REFERENCES bough_repos(id), principal TEXT NOT NULL, record JSONB NOT NULL, PRIMARY KEY(repo,principal));
CREATE TABLE IF NOT EXISTS bough_reviews (repo TEXT NOT NULL, revision TEXT NOT NULL, reviewer TEXT NOT NULL, record JSONB NOT NULL, PRIMARY KEY(repo,revision,reviewer), FOREIGN KEY(repo,revision) REFERENCES bough_changes(repo,id));`)
	return err
}
func retryableSQL(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && (e.Code == "40001" || e.Code == "23505")
}
func (s *SQLMetadata) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	for attempt := 0; attempt < 12; attempt++ {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err == nil {
			return nil
		}
		_ = tx.Rollback(ctx)
		if !retryableSQL(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1+rand.IntN(5*(attempt+1))) * time.Millisecond):
		}
	}
	return fail(503, "metadata transaction remained busy")
}
func (s *SQLMetadata) CreateRepo(ctx context.Context, c RepoConfig, genesis string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "INSERT INTO bough_repos(id,config) VALUES($1,$2)", c.ID, Encode(c)); e != nil {
			var p *pgconn.PgError
			if errors.As(e, &p) && p.Code == "23505" {
				return fail(409, "repository already exists")
			}
			return e
		}
		if _, e := tx.Exec(ctx, "INSERT INTO bough_changes(repo,id) VALUES($1,$2)", c.ID, genesis); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, "INSERT INTO bough_refs(repo,name,target) VALUES($1,'main',$2)", c.ID, genesis)
		return e
	})
}
func (s *SQLMetadata) Repo(ctx context.Context, id string) (RepoConfig, error) {
	var c RepoConfig
	var b []byte
	e := s.Pool.QueryRow(ctx, "SELECT config FROM bough_repos WHERE id=$1", id).Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return c, fail(404, "repository not found")
	}
	if e != nil {
		return c, e
	}
	e = jsonDecode(b, &c)
	return c, e
}
func (s *SQLMetadata) Exists(ctx context.Context, repo, id string) (bool, error) {
	var yes bool
	e := s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM bough_changes WHERE repo=$1 AND id=$2)", repo, id).Scan(&yes)
	return yes, e
}
func (s *SQLMetadata) Request(ctx context.Context, repo, key, fp string) (string, error) {
	var got, id string
	e := s.Pool.QueryRow(ctx, "SELECT fingerprint,revision FROM bough_requests WHERE repo=$1 AND key=$2", repo, key).Scan(&got, &id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	if got != fp {
		return "", fail(409, "request ID was used for different content")
	}
	return id, nil
}
func (s *SQLMetadata) Publish(ctx context.Context, repo, id, key, fp string, derived map[string]string) (result string, err error) {
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var got, existing string
		e := tx.QueryRow(ctx, "SELECT fingerprint,revision FROM bough_requests WHERE repo=$1 AND key=$2", repo, key).Scan(&got, &existing)
		if e == nil {
			if got != fp {
				return fail(409, "request ID was used for different content")
			}
			result = existing
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO bough_changes(repo,id) VALUES($1,$2) ON CONFLICT DO NOTHING", repo, id); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO bough_requests VALUES($1,$2,$3,$4)", repo, key, fp, id); e != nil {
			return e
		}
		for kind, object := range derived {
			if _, e = tx.Exec(ctx, "INSERT INTO bough_derived VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", repo, id, kind, object); e != nil {
				return e
			}
		}
		result = id
		return nil
	})
	return
}
func (s *SQLMetadata) Derived(ctx context.Context, repo, id, kind string) (string, error) {
	var object string
	e := s.Pool.QueryRow(ctx, "SELECT object FROM bough_derived WHERE repo=$1 AND revision=$2 AND kind=$3", repo, id, kind).Scan(&object)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	return object, e
}
func (s *SQLMetadata) SaveDerived(ctx context.Context, repo, id, kind, object string) error {
	_, e := s.Pool.Exec(ctx, "INSERT INTO bough_derived VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", repo, id, kind, object)
	return e
}
func (s *SQLMetadata) Ref(ctx context.Context, repo, name string) (string, error) {
	var target string
	e := s.Pool.QueryRow(ctx, "SELECT target FROM bough_refs WHERE repo=$1 AND name=$2", repo, name).Scan(&target)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", fail(404, "ref not found")
	}
	return target, e
}
func (s *SQLMetadata) CAS(ctx context.Context, repo, name, expected, target string, guards ...LandGuard) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		for _, guard := range guards {
			for reviewer, expected := range guard.ReviewerGrants {
				var raw []byte
				err := tx.QueryRow(ctx, "SELECT record FROM bough_grants WHERE repo=$1 AND principal=$2", repo, reviewer).Scan(&raw)
				actual := ""
				if err == nil {
					var principal Principal
					if err = jsonDecode(raw, &principal); err != nil {
						return err
					}
					actual = Hash(Encode(principal))
				} else if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if actual != expected {
					return fail(409, "reviewer permissions changed; retry landing")
				}
			}

			var raw []byte
			if err := tx.QueryRow(ctx, "SELECT config FROM bough_repos WHERE id=$1", repo).Scan(&raw); err != nil {
				return err
			}
			var config RepoConfig
			if err := jsonDecode(raw, &config); err != nil {
				return err
			}
			if Hash(Encode(config)) != guard.ConfigHash {
				return fail(409, "review policy changed; retry landing")
			}
			rows, err := tx.Query(ctx, "SELECT record FROM bough_reviews WHERE repo=$1 AND revision=$2", repo, guard.ReviewRevision)
			if err != nil {
				return err
			}
			reviews := []Review{}
			for rows.Next() {
				var b []byte
				var review Review
				if err = rows.Scan(&b); err != nil {
					rows.Close()
					return err
				}
				if err = jsonDecode(b, &review); err != nil {
					rows.Close()
					return err
				}
				reviews = append(reviews, review)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if ReviewHash(reviews) != guard.ReviewsHash {
				return fail(409, "reviews changed; retry landing")
			}
		}
		if expected == "" {
			_, e := tx.Exec(ctx, "INSERT INTO bough_refs VALUES($1,$2,$3)", repo, name, target)
			var p *pgconn.PgError
			if errors.As(e, &p) && p.Code == "23505" {
				return fail(409, "ref changed")
			}
			return e
		}
		result, e := tx.Exec(ctx, "UPDATE bough_refs SET target=$1 WHERE repo=$2 AND name=$3 AND target=$4", target, repo, name, expected)
		if e != nil {
			return e
		}
		if result.RowsAffected() != 1 {
			return fail(409, "ref changed")
		}
		return nil
	})
}
func (s *SQLMetadata) PutToken(ctx context.Context, hash string, r TokenRecord) error {
	_, e := s.Pool.Exec(ctx, "INSERT INTO bough_tokens VALUES($1,$2)", hash, Encode(r))
	return e
}
func (s *SQLMetadata) Token(ctx context.Context, hash string) (TokenRecord, error) {
	var r TokenRecord
	var b []byte
	e := s.Pool.QueryRow(ctx, "SELECT record FROM bough_tokens WHERE hash=$1", hash).Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return r, fail(401, "invalid token")
	}
	if e != nil {
		return r, e
	}
	e = jsonDecode(b, &r)
	return r, e
}
func (s *SQLMetadata) RevokeToken(ctx context.Context, hash string) error {
	_, e := s.Pool.Exec(ctx, "DELETE FROM bough_tokens WHERE hash=$1", hash)
	return e
}
func (s *SQLMetadata) Review(ctx context.Context, repo string, r Review) error {
	_, e := s.Pool.Exec(ctx, "INSERT INTO bough_reviews VALUES($1,$2,$3,$4) ON CONFLICT(repo,revision,reviewer) DO UPDATE SET record=excluded.record", repo, r.Revision, r.Reviewer, Encode(r))
	return e
}
func (s *SQLMetadata) Reviews(ctx context.Context, repo, id string) ([]Review, error) {
	rows, e := s.Pool.Query(ctx, "SELECT record FROM bough_reviews WHERE repo=$1 AND revision=$2", repo, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Review{}
	for rows.Next() {
		var b []byte
		var r Review
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = jsonDecode(b, &r); e != nil {
			return nil, e
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *SQLMetadata) SetPolicies(ctx context.Context, repo string, p []ReviewPolicy) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var b []byte
		if e := tx.QueryRow(ctx, "SELECT config FROM bough_repos WHERE id=$1", repo).Scan(&b); e != nil {
			return e
		}
		var c RepoConfig
		if e := jsonDecode(b, &c); e != nil {
			return e
		}
		c.Policies = p
		_, e := tx.Exec(ctx, "UPDATE bough_repos SET config=$1 WHERE id=$2", Encode(c), repo)
		return e
	})
}
func (s *SQLMetadata) ListChanges(ctx context.Context, repo, after string, limit int) ([]string, error) {
	rows, e := s.Pool.Query(ctx, "SELECT id FROM bough_changes WHERE repo=$1 AND id>$2 ORDER BY id LIMIT $3", repo, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MemoryMetadata provides deterministic, hermetic concurrency/fault tests. It is
// never selectable by the production command.
type MemoryMetadata struct {
	claims   map[string]bool
	grants   map[string]Principal
	mu       sync.Mutex
	repos    map[string]RepoConfig
	changes  map[string]bool
	requests map[string][2]string
	derived  map[string]string
	refs     map[string]string
	tokens   map[string]TokenRecord
	reviews  map[string]Review
}

func NewMemoryMetadata() *MemoryMetadata {
	return &MemoryMetadata{repos: map[string]RepoConfig{}, changes: map[string]bool{}, requests: map[string][2]string{}, derived: map[string]string{}, refs: map[string]string{}, tokens: map[string]TokenRecord{}, reviews: map[string]Review{}}
}
func key(parts ...string) string { return string(Encode(parts)) }
func (m *MemoryMetadata) CreateRepo(_ context.Context, c RepoConfig, g string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[c.ID]; ok {
		return fail(409, "repository exists")
	}
	m.repos[c.ID] = c
	m.changes[key(c.ID, g)] = true
	m.refs[key(c.ID, "main")] = g
	return nil
}
func (m *MemoryMetadata) Repo(_ context.Context, id string) (RepoConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[id]
	if !ok {
		return r, fail(404, "repository not found")
	}
	return r, nil
}
func (m *MemoryMetadata) Exists(_ context.Context, r, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changes[key(r, id)], nil
}
func (m *MemoryMetadata) Request(_ context.Context, r, k, fp string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.requests[key(r, k)]
	if ok && a[0] != fp {
		return "", fail(409, "request ID was used for different content")
	}
	return a[1], nil
}
func (m *MemoryMetadata) Publish(_ context.Context, r, id, k, fp string, d map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.requests[key(r, k)]
	if ok {
		if a[0] != fp {
			return "", fail(409, "request ID was used for different content")
		}
		return a[1], nil
	}
	m.changes[key(r, id)] = true
	m.requests[key(r, k)] = [2]string{fp, id}
	for kind, obj := range d {
		m.derived[key(r, id, kind)] = obj
	}
	return id, nil
}
func (m *MemoryMetadata) Derived(_ context.Context, r, id, k string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.derived[key(r, id, k)], nil
}
func (m *MemoryMetadata) SaveDerived(_ context.Context, r, id, k, o string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.derived[key(r, id, k)]; !ok {
		m.derived[key(r, id, k)] = o
	}
	return nil
}
func (m *MemoryMetadata) Ref(_ context.Context, r, n string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.refs[key(r, n)]
	if !ok {
		return "", fail(404, "ref not found")
	}
	return id, nil
}
func (m *MemoryMetadata) CAS(_ context.Context, r, n, e, t string, guards ...LandGuard) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, guard := range guards {
		for reviewer, expected := range guard.ReviewerGrants {
			actual := ""
			if principal, ok := m.grants[key(r, reviewer)]; ok {
				actual = Hash(Encode(principal))
			}
			if actual != expected {
				return fail(409, "reviewer permissions changed; retry landing")
			}
		}

		if Hash(Encode(m.repos[r])) != guard.ConfigHash {
			return fail(409, "review policy changed; retry landing")
		}
		reviews := []Review{}
		for k, v := range m.reviews {
			if k == key(r, guard.ReviewRevision, v.Reviewer) {
				reviews = append(reviews, v)
			}
		}
		if ReviewHash(reviews) != guard.ReviewsHash {
			return fail(409, "reviews changed; retry landing")
		}
	}
	if m.refs[key(r, n)] != e {
		return fail(409, "ref changed")
	}
	m.refs[key(r, n)] = t
	return nil
}
func (m *MemoryMetadata) PutToken(_ context.Context, h string, r TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[h] = r
	return nil
}
func (m *MemoryMetadata) Token(_ context.Context, h string) (TokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tokens[h]
	if !ok {
		return r, fail(401, "invalid token")
	}
	return r, nil
}
func (m *MemoryMetadata) RevokeToken(_ context.Context, h string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, h)
	return nil
}
func (m *MemoryMetadata) Review(_ context.Context, r string, v Review) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviews[key(r, v.Revision, v.Reviewer)] = v
	return nil
}
func (m *MemoryMetadata) Reviews(_ context.Context, r, id string) ([]Review, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := []Review{}
	for k, v := range m.reviews {
		if k == key(r, id, v.Reviewer) {
			all = append(all, v)
		}
	}
	return all, nil
}
func (m *MemoryMetadata) SetPolicies(_ context.Context, r string, p []ReviewPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.repos[r]
	if !ok {
		return fail(404, "repository not found")
	}
	c.Policies = p
	m.repos[r] = c
	return nil
}
func (m *MemoryMetadata) ListChanges(_ context.Context, r, after string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := []string{}
	for k := range m.changes {
		var p []string
		_ = jsonDecode([]byte(k), &p)
		if p[0] == r && p[1] > after {
			ids = append(ids, p[1])
		}
	}
	sortStrings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}
