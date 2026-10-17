package boughline

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

func (s *SQLMetadata) SetGrant(ctx context.Context, repo string, p Principal) error {
	_, err := s.Pool.Exec(ctx, "INSERT INTO bough_grants VALUES($1,$2,$3) ON CONFLICT(repo,principal) DO UPDATE SET record=excluded.record", repo, p.Name, Encode(p))
	return err
}
func (s *SQLMetadata) Grant(ctx context.Context, repo, name string) (Principal, error) {
	var p Principal
	var b []byte
	err := s.Pool.QueryRow(ctx, "SELECT record FROM bough_grants WHERE repo=$1 AND principal=$2", repo, name).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fail(404, "principal grant not found")
	}
	if err != nil {
		return p, err
	}
	err = jsonDecode(b, &p)
	return p, err
}
func (m *MemoryMetadata) SetGrant(_ context.Context, repo string, p Principal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grants == nil {
		m.grants = map[string]Principal{}
	}
	m.grants[key(repo, p.Name)] = p
	return nil
}
func (m *MemoryMetadata) Grant(_ context.Context, repo, name string) (Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.grants[key(repo, name)]
	if !ok {
		return p, fail(404, "principal grant not found")
	}
	return p, nil
}
