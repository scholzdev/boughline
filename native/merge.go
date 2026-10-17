package boughline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (e *Engine) Graph(ctx context.Context, repo, id string) (Graph, error) {
	var g Graph
	obj, err := e.Deriver.Get(ctx, repo, id, GraphKind)
	if err != nil {
		return g, err
	}
	err = GetJSON(ctx, e.Objects, "graph", obj, &g)
	return g, err
}
func (e *Engine) Ancestor(ctx context.Context, repo, a, b string) (bool, error) {
	x, err := e.Graph(ctx, repo, a)
	if err != nil {
		return false, err
	}
	y, err := e.Graph(ctx, repo, b)
	if err != nil {
		return false, err
	}
	if x.Generation > y.Generation {
		return false, nil
	}
	if a == b {
		return true, nil
	}
	if y.Depth >= x.Depth {
		distance := y.Depth - x.Depth
		candidate := b
		g := y
		for bit := 0; distance > 0; bit++ {
			if distance&1 != 0 {
				if bit >= len(g.Jumps) {
					break
				}
				candidate = g.Jumps[bit]
				g, err = e.Graph(ctx, repo, candidate)
				if err != nil {
					return false, err
				}
			}
			distance >>= 1
		}
		if candidate == a {
			return true, nil
		}
	}
	if err = ValidID(y.Ancestors); err != nil {
		return false, fail(500, "missing reachability index")
	}
	entry, err := LookupEntry(ctx, e.Objects, y.Ancestors, a)
	return entry != nil, err
}

func (e *Engine) MergeBases(ctx context.Context, repo, left, right string) ([]string, error) {
	for _, id := range []string{left, right} {
		if _, err := e.Change(ctx, repo, id); err != nil {
			return nil, err
		}
	}
	if yes, err := e.Ancestor(ctx, repo, left, right); err != nil {
		return nil, err
	} else if yes {
		return []string{left}, nil
	}
	if yes, err := e.Ancestor(ctx, repo, right, left); err != nil {
		return nil, err
	} else if yes {
		return []string{right}, nil
	}
	leftGraph, err := e.Graph(ctx, repo, left)
	if err != nil {
		return nil, err
	}
	rightGraph, err := e.Graph(ctx, repo, right)
	if err != nil {
		return nil, err
	}
	if leftGraph.Generation < rightGraph.Generation {
		left, right = right, left
		leftGraph = rightGraph
	}
	// Walk only ancestry exclusive to one side. An indexed common node is a
	// frontier: its parents cannot be maximal merge bases along this path.
	seen := map[string]bool{}
	todo := []string{right}
	candidates := []string{}
	for len(todo) > 0 {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		common := id == left
		if !common {
			entry, err := LookupEntry(ctx, e.Objects, leftGraph.Ancestors, id)
			if err != nil {
				return nil, err
			}
			common = entry != nil
		}
		if common {
			candidates = append(candidates, id)
			continue
		}
		change, err := e.change(ctx, id)
		if err != nil {
			return nil, err
		}
		todo = append(todo, change.Parents...)
	}
	bases := []string{}
	for _, c := range candidates {
		best := true
		for _, other := range candidates {
			if c == other {
				continue
			}
			yes, err := e.Ancestor(ctx, repo, c, other)
			if err != nil {
				return nil, err
			}
			if yes {
				best = false
				break
			}
		}
		if best {
			bases = append(bases, c)
		}
	}
	sort.Strings(bases)
	return bases, nil
}
func (e *Engine) mergeDelta(ctx context.Context, repo, base, left, right string, p Principal) (map[string]*File, error) {
	internal := Principal{Admin: true}
	ours, err := e.Diff(ctx, repo, base, left, internal)
	if err != nil {
		return nil, err
	}
	theirs, err := e.Diff(ctx, repo, base, right, internal)
	if err != nil {
		return nil, err
	}
	changes := map[string]*File{}
	conflicts := []string{}
	for filename, incoming := range theirs {
		if err = p.Check(filename, true); err != nil {
			return nil, fail(403, "merge includes paths outside your write permissions")
		}
		other, original := incoming.After, incoming.Before
		current := original
		own, changed := ours[filename]
		if changed {
			current = own.After
		}
		if sameFile(current, other) {
			continue
		}
		if !changed {
			changes[filename] = other
			continue
		}
		if original == nil || current == nil || other == nil {
			conflicts = append(conflicts, filename)
			continue
		}
		mode := current.Mode
		if mode != other.Mode {
			if mode == original.Mode {
				mode = other.Mode
			} else if other.Mode != original.Mode {
				conflicts = append(conflicts, filename)
				continue
			}
		}
		blob := ""
		switch {
		case current.ID == other.ID:
			blob = current.ID
		case current.ID == original.ID:
			blob = other.ID
		case other.ID == original.ID:
			blob = current.ID
		default:
			if mode == Symlink {
				conflicts = append(conflicts, filename)
				continue
			}
			blob, err = e.mergeFile(ctx, original.ID, current.ID, other.ID)
			if err != nil {
				return nil, err
			}
		}
		if blob == "" {
			conflicts = append(conflicts, filename)
		} else {
			raw, err := GetRaw(ctx, e.Objects, "blob", blob)
			if err != nil {
				return nil, err
			}
			changes[filename] = &File{ID: blob, Mode: mode, Size: int64(len(raw))}
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fail(409, "merge conflict", map[string]any{"paths": conflicts, "base": base, "left": left, "right": right})
	}
	return changes, nil
}
func (e *Engine) mergeFile(ctx context.Context, base, left, right string) (string, error) {
	dir, err := os.MkdirTemp("", "bough-merge-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	names := []string{}
	for i, id := range []string{left, base, right} {
		data, err := GetRaw(ctx, e.Objects, "blob", id)
		if err != nil {
			return "", err
		}
		if strings.ContainsRune(string(data), 0) {
			return "", nil
		}
		name := filepath.Join(dir, []string{"ours", "base", "theirs"}[i])
		if err = os.WriteFile(name, data, 0600); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	mergeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(mergeCtx, "git", append([]string{"merge-file", "-p", "--diff3"}, names...)...)
	out, err := cmd.Output()
	if err == nil {
		return PutRaw(ctx, e.Objects, "blob", out)
	}
	if mergeCtx.Err() != nil {
		return "", fail(503, "text merge timed out")
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() > 0 {
		return "", nil
	}
	return "", err
}
func (e *Engine) Merge(ctx context.Context, repo, left, right, message, request string, p Principal) (string, error) {
	bases, err := e.MergeBases(ctx, repo, left, right)
	if err != nil {
		return "", err
	}
	if len(bases) != 1 {
		return "", fail(409, "multiple merge bases need explicit resolution", bases)
	}
	if bases[0] == right {
		return left, nil
	}
	if bases[0] == left {
		changes, err := e.Diff(ctx, repo, left, right, Principal{Admin: true})
		if err != nil {
			return "", err
		}
		for filename := range changes {
			if err = p.Check(filename, true); err != nil {
				return "", fail(403, "merge includes paths outside your write permissions")
			}
		}
		return right, nil
	}
	changes, err := e.mergeDelta(ctx, repo, bases[0], left, right, p)
	if err != nil {
		return "", err
	}
	if err = e.claimMergeResult(ctx, repo, p, changes); err != nil {
		return "", err
	}
	return e.Commit(ctx, repo, p, CommitRequest{Parents: []string{left, right}, Changes: changes, Message: message, RequestID: request})
}
func (e *Engine) Rebase(ctx context.Context, repo, revision, onto, request string, p Principal) (string, error) {
	c, err := e.Change(ctx, repo, revision)
	if err != nil {
		return "", err
	}
	if len(c.Parents) == 0 {
		return "", fail(400, "cannot rebase genesis")
	}
	if _, err = e.Change(ctx, repo, onto); err != nil {
		return "", err
	}
	changes, err := e.mergeDelta(ctx, repo, c.Parents[0], onto, revision, p)
	if err != nil {
		return "", err
	}
	if err = e.claimMergeResult(ctx, repo, p, changes); err != nil {
		return "", err
	}
	return e.Commit(ctx, repo, p, CommitRequest{Parents: []string{onto}, Changes: changes, Message: c.Message, RequestID: request, RebasedFrom: revision})
}
