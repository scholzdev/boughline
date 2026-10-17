package boughline

import (
	"context"
	"strings"
)

type objectRef struct{ Kind, ID string }

func derivedObjectKind(kind string) (string, error) {
	switch {
	case kind == ManifestKind || strings.HasPrefix(kind, "access-view/"):
		return "tree:blob", nil
	case kind == HistoryKind:
		return "tree:history", nil
	case kind == CopyKind || kind == SearchKind:
		return "tree:path", nil
	case kind == GraphKind || kind == "graph/v1":
		return "graph", nil
	case strings.HasPrefix(kind, blamePrefix):
		return "blame", nil
	}
	return "", fail(400, "unknown stored derived format", kind)
}
func verifyObjects(ctx context.Context, store ObjectStore, seeds []objectRef) (int, error) {
	seen := map[objectRef]bool{}
	queue := append([]objectRef{}, seeds...)
	for len(queue) > 0 {
		ref := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		kind, fileKind, _ := strings.Cut(ref.Kind, ":")
		raw, err := GetRaw(ctx, store, kind, ref.ID)
		if err != nil {
			return 0, err
		}
		add := func(kind, id string) {
			if id != "" {
				queue = append(queue, objectRef{kind, id})
			}
		}
		switch kind {
		case "tree":
			var tree Tree
			if err = jsonDecode(raw, &tree); err != nil {
				return 0, err
			}
			for name, entry := range tree {
				if entry.Kind == "tree" || entry.Kind == "shard" {
					add(ref.Kind, entry.ID)
				} else if entry.Kind == "file" {
					add(fileKind, entry.ID)
				} else {
					return 0, fail(500, "invalid stored tree entry", name)
				}
			}
		case "changeset":
			var c Changeset
			if err = jsonDecode(raw, &c); err != nil {
				return 0, err
			}
			for _, p := range c.Parents {
				add("changeset", p)
			}
			for _, f := range c.Changes {
				if f != nil {
					add("blob", f.ID)
				}
			}
		case "history":
			var node HistoryNode
			if err = jsonDecode(raw, &node); err != nil {
				return 0, err
			}
			add("changeset", node.Commit)
			add("history", node.Previous)
			if node.File != nil {
				add("blob", node.File.ID)
			}
		case "graph":
			var graph Graph
			if err = jsonDecode(raw, &graph); err != nil {
				return 0, err
			}
			add("tree:changeset", graph.Ancestors)
			for _, id := range graph.Jumps {
				add("changeset", id)
			}
		case "blame":
			var blame Blame
			if err = jsonDecode(raw, &blame); err != nil {
				return 0, err
			}
			add("blob", blame.Blob)
			for _, origin := range blame.Origins {
				add("changeset", origin.Commit)
			}
		case "path":
			var filename string
			if err = jsonDecode(raw, &filename); err != nil {
				return 0, err
			}
			if err = ValidPath(filename, false); err != nil {
				return 0, err
			}
		case "blob":
		default:
			return 0, fail(500, "unknown referenced object kind", kind)
		}
	}
	return len(seen), nil
}
func Verify(ctx context.Context, db *SQLMetadata, store ObjectStore) (int, error) {
	seeds := []objectRef{}
	claimRows, err := db.Pool.Query(ctx, "SELECT id FROM bough_blob_claims")
	if err != nil {
		return 0, err
	}
	for claimRows.Next() {
		var id string
		if err = claimRows.Scan(&id); err != nil {
			claimRows.Close()
			return 0, err
		}
		seeds = append(seeds, objectRef{"blob", id})
	}
	err = claimRows.Err()
	claimRows.Close()
	if err != nil {
		return 0, err
	}
	rows, err := db.Pool.Query(ctx, "SELECT id FROM bough_changes")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		seeds = append(seeds, objectRef{"changeset", id})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	rows, err = db.Pool.Query(ctx, "SELECT kind,object FROM bough_derived")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var kind, id string
		if err = rows.Scan(&kind, &id); err != nil {
			rows.Close()
			return 0, err
		}
		kind, err = derivedObjectKind(kind)
		if err != nil {
			rows.Close()
			return 0, err
		}
		seeds = append(seeds, objectRef{kind, id})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	return verifyObjects(ctx, store, seeds)
}
