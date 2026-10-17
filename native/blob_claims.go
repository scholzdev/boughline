package boughline

import "context"

// Immutable blob hashes are global storage identities, not authorization. A
// principal must supply the bytes to this repository, or use an authorized
// server-side merge/rebase, before a commit may introduce a content reference.
func (e *Engine) UploadBlob(ctx context.Context, repo string, p Principal, raw []byte) (string, error) {
	if _, err := e.Meta.Repo(ctx, repo); err != nil {
		return "", err
	}
	if !p.Admin {
		writable := false
		for _, rule := range p.Rules {
			writable = writable || rule.Write
		}
		if !writable {
			return "", fail(403, "write permission required")
		}
	}
	id, err := PutRaw(ctx, e.Objects, "blob", raw)
	if err != nil {
		return "", err
	}
	if err = e.Meta.ClaimBlob(ctx, repo, p.Name, id); err != nil {
		return "", err
	}
	return id, nil
}
func (s *SQLMetadata) ClaimBlob(ctx context.Context, repo, principal, id string) error {
	_, err := s.Pool.Exec(ctx, "INSERT INTO bough_blob_claims (repo,principal,id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING", repo, principal, id)
	return err
}
func (s *SQLMetadata) HasBlobClaim(ctx context.Context, repo, principal, id string) (bool, error) {
	var found bool
	err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM bough_blob_claims WHERE repo=$1 AND principal=$2 AND id=$3)", repo, principal, id).Scan(&found)
	return found, err
}
func (m *MemoryMetadata) ClaimBlob(_ context.Context, repo, principal, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[repo]; !ok {
		return fail(404, "repository not found")
	}
	if m.claims == nil {
		m.claims = map[string]bool{}
	}
	m.claims[key(repo, principal, id)] = true
	return nil
}
func (m *MemoryMetadata) HasBlobClaim(_ context.Context, repo, principal, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claims[key(repo, principal, id)], nil
}
func (e *Engine) claimMergeResult(ctx context.Context, repo string, p Principal, changes map[string]*File) error {
	for filename, file := range changes {
		if p.Check(filename, true) != nil {
			return fail(403, "merge includes paths outside your write permissions")
		}
		if file != nil {
			if err := e.Meta.ClaimBlob(ctx, repo, p.Name, file.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
