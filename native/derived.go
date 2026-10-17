package boughline

import (
	"context"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

const ManifestKind = "manifest/v1"
const HistoryKind = "path-history/v1"
const GraphKind = "graph/v2"
const blamePrefix = "blame/v1:"

// One recursive mechanism handles every derived value, including lazy per-path
// blame. Concurrent servers may compute the same immutable value; publication is
// idempotent and requires no derivation lease or distributed application lock.
type Deriver struct{ Engine *Engine }

func (d *Deriver) Get(ctx context.Context, repo, id, kind string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cached, err := d.Engine.Meta.Derived(ctx, repo, id, kind)
	if err != nil || cached != "" {
		return cached, err
	}
	c, err := d.Engine.Change(ctx, repo, id)
	if err != nil {
		return "", err
	}
	object, err := d.Build(ctx, repo, id, c, kind)
	if err != nil {
		return "", err
	}
	if err = d.Engine.Meta.SaveDerived(ctx, repo, id, kind, object); err != nil {
		return "", err
	}
	return object, nil
}
func (d *Deriver) parents(ctx context.Context, repo string, c Changeset, kind string) ([]string, error) {
	objects := make([]string, 0, len(c.Parents))
	for _, parent := range c.Parents {
		object, err := d.Get(ctx, repo, parent, kind)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, nil
}
func (d *Deriver) Build(ctx context.Context, repo, id string, c Changeset, kind string) (string, error) {
	e := d.Engine
	if kind == CopyKind || kind == SearchKind {
		return d.pathIndex(ctx, repo, c, kind)
	}
	if strings.HasPrefix(kind, blamePrefix) {
		return d.blame(ctx, repo, id, c, strings.TrimPrefix(kind, blamePrefix))
	}
	parents, err := d.parents(ctx, repo, c, kind)
	if err != nil {
		return "", err
	}
	switch kind {
	case ManifestKind:
		if len(parents) == 0 {
			return PutJSON(ctx, e.Objects, "tree", Tree{})
		}
		return e.apply(ctx, parents[0], c.Changes)
	case HistoryKind:
		root := ""
		if len(parents) == 0 {
			root, err = PutJSON(ctx, e.Objects, "tree", Tree{})
		} else {
			root = parents[0]
		}
		if err != nil {
			return "", err
		}
		updates := map[string]*File{}
		for filename, f := range c.Changes {
			old, err := e.lookup(ctx, root, historyPath(filename))
			if err != nil {
				return "", err
			}
			previous := ""
			if old != nil && old.Kind == "file" {
				previous = old.ID
			}
			node := HistoryNode{Commit: id, Previous: previous, File: f}
			identity, err := PutJSON(ctx, e.Objects, "history", node)
			if err != nil {
				return "", err
			}
			updates[filename] = &File{ID: identity, Mode: Regular}
		}
		// History is a path trie that also retains deleted paths. A file can become a
		// directory, so path history uses escaped components and a reserved leaf.
		escaped := map[string]*File{}
		for p, f := range updates {
			escaped[historyPath(p)] = f
		}
		return e.apply(ctx, root, escaped)
	case GraphKind:
		g := Graph{Jumps: []string{}}
		if len(parents) > 0 {
			for _, obj := range parents {
				var pg Graph
				if err = GetJSON(ctx, e.Objects, "graph", obj, &pg); err != nil {
					return "", err
				}
				if pg.Generation >= g.Generation {
					g.Generation = pg.Generation + 1
				}
			}
			var first Graph
			if err = GetJSON(ctx, e.Objects, "graph", parents[0], &first); err != nil {
				return "", err
			}
			g.Depth = first.Depth + 1
			g.Jumps = append(g.Jumps, c.Parents[0])
			ancestor := first
			for level := 0; level < len(ancestor.Jumps); level++ {
				next := ancestor.Jumps[level]
				g.Jumps = append(g.Jumps, next)
				obj, err := d.Get(ctx, repo, next, GraphKind)
				if err != nil {
					return "", err
				}
				if err = GetJSON(ctx, e.Objects, "graph", obj, &ancestor); err != nil {
					return "", err
				}
			}
		}
		if len(parents) == 0 {
			g.Ancestors, err = PutJSON(ctx, e.Objects, "tree", Tree{})
		} else {
			var first Graph
			if err = GetJSON(ctx, e.Objects, "graph", parents[0], &first); err != nil {
				return "", err
			}
			if err = ValidID(first.Ancestors); err != nil {
				return "", fail(500, "missing reachability index in parent graph")
			}
			g.Ancestors, err = setEntry(ctx, e.Objects, first.Ancestors, c.Parents[0], &Entry{Kind: "file", ID: c.Parents[0], Mode: Regular}, 0)
			if err != nil {
				return "", err
			}
			// The first parent's complete set is shared. Only walk ancestry brought
			// in by additional parents until reaching that already indexed frontier.
			todo := append([]string{}, c.Parents[1:]...)
			for len(todo) > 0 {
				parent := todo[len(todo)-1]
				todo = todo[:len(todo)-1]
				entry, err := LookupEntry(ctx, e.Objects, g.Ancestors, parent)
				if err != nil {
					return "", err
				}
				if entry != nil {
					continue
				}
				g.Ancestors, err = setEntry(ctx, e.Objects, g.Ancestors, parent, &Entry{Kind: "file", ID: parent, Mode: Regular}, 0)
				if err != nil {
					return "", err
				}
				change, err := e.change(ctx, parent)
				if err != nil {
					return "", err
				}
				todo = append(todo, change.Parents...)
			}
		}
		if err != nil {
			return "", err
		}
		return PutJSON(ctx, e.Objects, "graph", g)
	default:
		return "", fail(400, "unknown derived index", kind)
	}
}
func historyPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = Hash([]byte(s))
	}
	return strings.Join(parts, "/") + "/!"
}
func (d *Deriver) blame(ctx context.Context, repo, id string, c Changeset, filename string) (string, error) {
	e := d.Engine
	if err := ValidPath(filename, false); err != nil {
		return "", err
	}
	root, err := d.Get(ctx, repo, id, ManifestKind)
	if err != nil {
		return "", err
	}
	entry, err := e.Lookup(ctx, root, filename)
	if err != nil {
		return "", err
	}
	if entry == nil || entry.Kind != "file" {
		return "", fail(404, "file not found")
	}
	historyRoot, err := d.Get(ctx, repo, id, HistoryKind)
	if err != nil {
		return "", err
	}
	recent, err := e.lookup(ctx, historyRoot, historyPath(filename))
	if err != nil {
		return "", err
	}
	if recent != nil {
		var h HistoryNode
		if err = GetJSON(ctx, e.Objects, "history", recent.ID, &h); err != nil {
			return "", err
		}
		if h.Commit != id {
			return d.Get(ctx, repo, h.Commit, blamePrefix+filename)
		}
	}
	data, err := GetRaw(ctx, e.Objects, "blob", entry.ID)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(string(data), 0) {
		return "", fail(422, "binary files do not have line blame")
	}
	lines := sourceLines(string(data))
	if len(lines) > 200000 {
		return "", fail(413, "file exceeds line-blame limit")
	}
	result := Blame{Blob: entry.ID, Origins: make([]Origin, len(lines))}
	for i := range result.Origins {
		result.Origins[i] = Origin{Commit: id, Line: i + 1}
	}
	for _, parent := range c.Parents {
		parentRoot, err := e.Root(ctx, repo, parent)
		if err != nil {
			return "", err
		}
		old, err := e.Lookup(ctx, parentRoot, filename)
		if err != nil {
			return "", err
		}
		if old != nil && old.Kind == "file" {
			obj, err := d.Get(ctx, repo, parent, blamePrefix+filename)
			if err != nil {
				return "", err
			}
			var previous Blame
			if err = GetJSON(ctx, e.Objects, "blame", obj, &previous); err != nil {
				return "", err
			}
			raw, err := GetRaw(ctx, e.Objects, "blob", old.ID)
			if err != nil {
				return "", err
			}
			oldLines := sourceLines(string(raw))
			matcher := difflib.NewMatcher(oldLines, lines)
			for _, op := range matcher.GetOpCodes() {
				if op.Tag == 'e' {
					for n := 0; n < op.J2-op.J1; n++ {
						if op.I1+n >= len(previous.Origins) {
							return "", fmt.Errorf("corrupt blame index")
						}
						if result.Origins[op.J1+n].Commit == id {
							result.Origins[op.J1+n] = previous.Origins[op.I1+n]
						}
					}
				}
			}
		}
	}
	return PutJSON(ctx, e.Objects, "blame", result)
}
func (e *Engine) Blame(ctx context.Context, repo, id, filename string, p Principal) (Blame, error) {
	var result Blame
	if err := p.Check(filename, false); err != nil {
		return result, err
	}
	obj, err := e.Deriver.Get(ctx, repo, id, blamePrefix+filename)
	if err != nil {
		return result, err
	}
	err = GetJSON(ctx, e.Objects, "blame", obj, &result)
	return result, err
}
