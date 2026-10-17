package boughline

import (
	"context"
	"log"
)

// DeriveIndices is an administrator's resumable prewarming job. Reading missing
// changes in publication order keeps cold histories off latency-sensitive API
// requests. Each completed value is durable; reruns skip completed derivations.
func DeriveIndices(ctx context.Context, db *SQLMetadata, store ObjectStore, repo, kind string) (map[string]int, error) {
	if _, err := db.Repo(ctx, repo); err != nil {
		return nil, err
	}
	kinds := []string{kind}
	if kind == "all" {
		kinds = []string{ManifestKind, GraphKind, HistoryKind, CopyKind, SearchKind}
	}
	for _, kind := range kinds {
		switch kind {
		case ManifestKind, GraphKind, HistoryKind, CopyKind, SearchKind:
		default:
			return nil, fail(400, "unsupported prewarming index", kind)
		}
	}
	engine := NewEngine(db, NewCachedObjects(store, 256<<20))
	processed := map[string]int{}
	for _, kind := range kinds {
		rows, err := db.Pool.Query(ctx, "SELECT c.id FROM bough_changes c LEFT JOIN bough_derived d ON d.repo=c.repo AND d.revision=c.id AND d.kind=$2 WHERE c.repo=$1 AND d.object IS NULL ORDER BY c.created_at,c.id", repo, kind)
		if err != nil {
			return nil, err
		}
		processed[kind] = 0
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			if _, err = engine.Deriver.Get(ctx, repo, id, kind); err != nil {
				break
			}
			processed[kind]++
			if processed[kind]%1000 == 0 {
				log.Printf("derived %s: %d changesets", kind, processed[kind])
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return processed, err
		}
	}
	return processed, nil
}
