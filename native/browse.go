package boughline

import (
	"context"
	"sort"
)

type BrowseEntry struct {
	Name  string `json:"name"`
	Entry Entry  `json:"entry"`
}
type BrowseResult struct {
	Revision  string        `json:"revision"`
	Path      string        `json:"path"`
	Entries   []BrowseEntry `json:"entries"`
	Next      string        `json:"next"`
	File      *File         `json:"file,omitempty"`
	Content   []byte        `json:"content,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

func (e *Engine) Browse(ctx context.Context, repo, revision, path, cursor string, limit int, p Principal) (BrowseResult, error) {
	out := BrowseResult{Revision: revision, Path: path, Entries: []BrowseEntry{}}
	if err := ValidPath(path, true); err != nil {
		return out, err
	}
	if limit < 1 || limit > 500 {
		return out, fail(400, "browse limit must be 1..500")
	}
	root, err := e.Root(ctx, repo, revision)
	if err != nil {
		return out, err
	}
	if !p.Admin {
		root, err = e.accessTree(ctx, root, "", p.Rules)
		if err != nil {
			return out, err
		}
	}
	entry, err := e.Lookup(ctx, root, path)
	if err != nil {
		return out, err
	}
	if entry == nil {
		return out, fail(404, "path not found")
	}
	if entry.Kind == "file" {
		if err = p.Check(path, false); err != nil {
			return out, err
		}
		out.File = &File{ID: entry.ID, Mode: entry.Mode, Size: entry.Size}
		// File browser stays bounded. Large binary objects use the object hierarchy.
		if entry.Size > 1<<20 {
			out.Truncated = true
			return out, nil
		}
		out.Content, err = GetRaw(ctx, e.Objects, "blob", entry.ID)
		return out, err
	}
	// Retain only the next bounded page while walking physical pages; memory does
	// not grow with directory size. Object clients can use direct hash-page lookup.
	err = WalkEntries(ctx, e.Objects, entry.ID, func(name string, entry Entry) error {
		if name <= cursor {
			return nil
		}
		out.Entries = append(out.Entries, BrowseEntry{Name: name, Entry: entry})
		if len(out.Entries) > limit+1 {
			sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
			out.Entries = out.Entries[:limit+1]
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
		out.Next = out.Entries[limit-1].Name
	}
	return out, nil
}
