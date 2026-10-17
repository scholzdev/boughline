package boughline

import (
	"context"
	"sort"
	"strings"
)

const CopyKind = "copy-paths/v1"
const SearchKind = "path-search/v1"

func pathTokens(filename string) []string {
	runes := []rune(strings.ToLower(filename))
	tokens := map[string]bool{}
	for i := 0; i+3 <= len(runes); i++ {
		tokens[string(runes[i:i+3])] = true
	}
	out := []string{}
	for token := range tokens {
		out = append(out, Hash([]byte(token)))
	}
	sort.Strings(out)
	return out
}

// Both indices are persistent Merkle tries maintained from the first parent's
// value and this changeset's delta. No repository scan or mutable search service.
func (d *Deriver) pathIndex(ctx context.Context, repo string, c Changeset, kind string) (string, error) {
	e := d.Engine
	root, err := PutJSON(ctx, e.Objects, "tree", Tree{})
	if err != nil {
		return "", err
	}
	parentManifest := ""
	if len(c.Parents) > 0 {
		root, err = d.Get(ctx, repo, c.Parents[0], kind)
		if err != nil {
			return "", err
		}
		parentManifest, err = e.Root(ctx, repo, c.Parents[0])
		if err != nil {
			return "", err
		}
	}
	updates := map[string]*File{}
	for filename, file := range c.Changes {
		var old *Entry
		if parentManifest != "" {
			old, err = e.Lookup(ctx, parentManifest, filename)
			if err != nil {
				return "", err
			}
		}
		keys := func(id string) []string {
			if kind == CopyKind {
				return []string{id}
			}
			return pathTokens(filename)
		}
		pathKey := Hash([]byte(filename))
		if old != nil {
			for _, token := range keys(old.ID) {
				updates[token+"/"+pathKey] = nil
			}
		}
		if file != nil {
			id, err := PutJSON(ctx, e.Objects, "path", filename)
			if err != nil {
				return "", err
			}
			for _, token := range keys(file.ID) {
				updates[token+"/"+pathKey] = &File{ID: id, Mode: Regular}
			}
		}
	}
	return e.apply(ctx, root, updates)
}

func (e *Engine) indexPaths(ctx context.Context, root, key string, p Principal, match func(string) bool, limit int) ([]string, error) {
	entry, err := e.Lookup(ctx, root, key)
	if err != nil {
		return nil, err
	}
	out := []string{}
	if entry == nil {
		return out, nil
	}
	err = WalkEntries(ctx, e.Objects, entry.ID, func(_ string, entry Entry) error {
		var filename string
		if err := GetJSON(ctx, e.Objects, "path", entry.ID, &filename); err != nil {
			return err
		}
		if p.Check(filename, false) == nil && match(filename) {
			out = append(out, filename)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (e *Engine) SearchPaths(ctx context.Context, repo, revision, query string, limit int, p Principal) ([]string, error) {
	if len([]rune(query)) < 3 || len(query) > 1024 || limit < 1 || limit > 1000 {
		return nil, fail(400, "path search requires 3..1024 characters and limit 1..1000")
	}
	root, err := e.Deriver.Get(ctx, repo, revision, SearchKind)
	if err != nil {
		return nil, err
	}
	rarest := ""
	var count int64
	for _, token := range pathTokens(query) {
		entry, err := e.Lookup(ctx, root, token)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			return []string{}, nil
		}
		if rarest == "" || entry.Count < count {
			rarest = token
			count = entry.Count
		}
	}
	return e.indexPaths(ctx, root, rarest, p, func(filename string) bool { return strings.Contains(strings.ToLower(filename), strings.ToLower(query)) }, limit)
}

func (e *Engine) Copies(ctx context.Context, repo, revision, filename string, limit int, p Principal) ([]string, error) {
	if err := ValidPath(filename, false); err != nil {
		return nil, err
	}
	if err := p.Check(filename, false); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, fail(400, "copy limit must be 1..1000")
	}
	manifest, err := e.Root(ctx, repo, revision)
	if err != nil {
		return nil, err
	}
	file, err := e.Lookup(ctx, manifest, filename)
	if err != nil {
		return nil, err
	}
	if file == nil || file.Kind != "file" {
		return nil, fail(404, "file not found")
	}
	root, err := e.Deriver.Get(ctx, repo, revision, CopyKind)
	if err != nil {
		return nil, err
	}
	return e.indexPaths(ctx, root, file.ID, p, func(candidate string) bool { return candidate != filename }, limit)
}
