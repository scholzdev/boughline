package boughline

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Engine struct {
	Meta    Metadata
	Objects ObjectStore
	Deriver *Deriver
}

func NewEngine(m Metadata, o ObjectStore) *Engine {
	e := &Engine{Meta: m, Objects: o}
	e.Deriver = &Deriver{Engine: e}
	return e
}
func (e *Engine) CreateRepo(ctx context.Context, c RepoConfig) (string, error) {
	if err := ValidName(c.ID); err != nil {
		return "", err
	}
	if err := ValidText(c.Owner, 128); err != nil {
		return "", err
	}
	if err := ValidatePolicies(c.Policies); err != nil {
		return "", err
	}
	g := Changeset{Version: 1, Parents: []string{}, Changes: map[string]*File{}, Author: c.Owner, Message: "Repository initialized", TimeNS: 0}
	id, err := PutJSON(ctx, e.Objects, "changeset", g)
	if err != nil {
		return "", err
	}
	if err = e.Meta.CreateRepo(ctx, c, id); err != nil {
		return "", err
	}
	return id, nil
}
func (e *Engine) Change(ctx context.Context, repo, id string) (Changeset, error) {
	var c Changeset
	if err := ValidID(id); err != nil {
		return c, err
	}
	yes, err := e.Meta.Exists(ctx, repo, id)
	if err != nil {
		return c, err
	}
	if !yes {
		return c, fail(404, "changeset not found")
	}
	return e.change(ctx, id)
}
func (e *Engine) change(ctx context.Context, id string) (Changeset, error) {
	var c Changeset
	err := GetJSON(ctx, e.Objects, "changeset", id, &c)
	if err == nil && c.Version != 1 {
		return c, fail(500, "unsupported stored changeset version")
	}
	return c, err
}
func (e *Engine) Tree(ctx context.Context, id string) (Tree, error) {
	return flatTree(ctx, e.Objects, id)
}
func (e *Engine) Root(ctx context.Context, repo, id string) (string, error) {
	return e.Deriver.Get(ctx, repo, id, ManifestKind)
}
func (e *Engine) Lookup(ctx context.Context, root, p string) (*Entry, error) {
	if err := ValidPath(p, true); err != nil {
		return nil, err
	}
	return e.lookup(ctx, root, p)
}
func (e *Engine) lookup(ctx context.Context, root, p string) (*Entry, error) {
	if p == "" {
		return &Entry{Kind: "tree", ID: root}, nil
	}
	parts := strings.Split(p, "/")
	for i, name := range parts {
		entry, err := LookupEntry(ctx, e.Objects, root, name)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			return nil, nil
		}
		if i == len(parts)-1 {
			return entry, nil
		}
		if entry.Kind != "tree" {
			return nil, nil
		}
		root = entry.ID
	}
	return nil, nil
}
func (e *Engine) rewrite(ctx context.Context, root string, changes map[string]*File) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	updates := map[string]*Entry{}
	groups := map[string]map[string]*File{}
	for p, v := range changes {
		name, rest, _ := strings.Cut(p, "/")
		if groups[name] == nil {
			groups[name] = map[string]*File{}
		}
		groups[name][rest] = v
	}
	for name, group := range groups {
		old, err := LookupEntry(ctx, e.Objects, root, name)
		if err != nil {
			return "", err
		}
		if v, ok := group[""]; ok {
			if len(group) != 1 {
				return "", fail(409, "file/directory collision", name)
			}
			if v == nil {
				updates[name] = nil
			} else {
				if old != nil && old.Kind == "tree" {
					return "", fail(409, "file/directory collision", name)
				}
				updates[name] = &Entry{Kind: "file", ID: v.ID, Mode: v.Mode, Size: v.Size}
			}
			continue
		}
		if old != nil && old.Kind != "tree" {
			return "", fail(409, "file/directory collision", name)
		}
		child := ""
		if old != nil {
			child = old.ID
		} else {
			child, err = PutJSON(ctx, e.Objects, "tree", Tree{})
			if err != nil {
				return "", err
			}
		}
		child, err = e.rewrite(ctx, child, group)
		if err != nil {
			return "", err
		}
		items, count, err := treeStats(ctx, e.Objects, child)
		if err != nil {
			return "", err
		}
		if items == 0 {
			updates[name] = nil
		} else {
			updates[name] = &Entry{Kind: "tree", ID: child, Items: items, Count: count}
		}
	}
	return patchEntries(ctx, e.Objects, root, updates)
}
func (e *Engine) apply(ctx context.Context, root string, changes map[string]*File) (string, error) {
	deletes, writes := map[string]*File{}, map[string]*File{}
	for p, v := range changes {
		if v == nil {
			deletes[p] = nil
		} else {
			writes[p] = v
		}
	}
	var err error
	if len(deletes) > 0 {
		root, err = e.rewrite(ctx, root, deletes)
		if err != nil {
			return "", err
		}
	}
	if len(writes) > 0 {
		root, err = e.rewrite(ctx, root, writes)
	}
	return root, err
}
func (e *Engine) Commit(ctx context.Context, repo string, p Principal, r CommitRequest) (string, error) {
	if err := ValidText(p.Name, 128); err != nil {
		return "", err
	}
	if err := ValidText(r.RequestID, 128); err != nil {
		return "", err
	}
	if err := ValidText(r.Message, 16384); err != nil {
		return "", err
	}
	if len(r.Parents) < 1 || len(r.Parents) > 64 {
		return "", fail(400, "one to 64 distinct parents are required")
	}
	seenParents := map[string]bool{}
	for _, parent := range r.Parents {
		if seenParents[parent] {
			return "", fail(400, "duplicate parent")
		}
		seenParents[parent] = true
	}
	if r.OriginalGit != nil {
		if !p.Admin {
			return "", fail(403, "administrator required to import provenance")
		}
		if len(r.OriginalGit.ID) != 40 && len(r.OriginalGit.ID) != 64 {
			return "", fail(400, "invalid Git provenance")
		}
		if _, err := hex.DecodeString(r.OriginalGit.ID); err != nil {
			return "", fail(400, "invalid Git provenance")
		}
		if len(r.OriginalGit.Raw) > 1<<20 {
			return "", fail(413, "Git provenance object exceeds limit")
		}
		envelope := append([]byte(fmt.Sprintf("commit %d%c", len(r.OriginalGit.Raw), 0)), r.OriginalGit.Raw...)
		identity := ""
		if len(r.OriginalGit.ID) == 40 {
			sum := sha1.Sum(envelope)
			identity = hex.EncodeToString(sum[:])
		} else {
			sum := sha256.Sum256(envelope)
			identity = hex.EncodeToString(sum[:])
		}
		if identity != r.OriginalGit.ID {
			return "", fail(400, "Git provenance identity mismatch")
		}
		parsed, _, message, _, err := parseGitCommit(identity, r.OriginalGit.Raw)
		if err != nil {
			return "", err
		}
		if string(Encode(parsed)) != string(Encode(r.OriginalGit)) || message != r.Message {
			return "", fail(400, "Git provenance metadata mismatch")
		}

		for _, text := range []string{r.OriginalGit.Author, r.OriginalGit.Committer, r.OriginalGit.AuthorTime, r.OriginalGit.CommitterTime} {
			if err := ValidText(text, 1024); err != nil {
				return "", err
			}
		}
	}

	if r.Changes == nil || len(r.Changes) > MaxChanges {
		return "", fail(400, "invalid changeset size")
	}
	for _, id := range r.Parents {
		if _, err := e.Change(ctx, repo, id); err != nil {
			return "", err
		}
	}
	for filename, v := range r.Changes {
		if err := ValidPath(filename, false); err != nil {
			return "", err
		}
		if err := p.Check(filename, true); err != nil {
			return "", err
		}
		if v != nil {
			if err := v.Validate(); err != nil {
				return "", err
			}
			claimed, err := e.Meta.HasBlobClaim(ctx, repo, p.Name, v.ID)
			if err != nil {
				return "", err
			}
			if !claimed {
				return "", fail(403, "blob must first be uploaded by this principal to this repository")
			}
			raw, err := GetRaw(ctx, e.Objects, "blob", v.ID)
			if err != nil {
				return "", err
			}
			if v.Mode == Symlink && strings.ContainsRune(string(raw), 0) {
				return "", fail(400, "invalid symlink target")
			}
			if int64(len(raw)) != v.Size {
				return "", fail(400, "blob size does not match file metadata", filename)
			}
		}
	}
	if r.RebasedFrom != "" {
		if _, err := e.Change(ctx, repo, r.RebasedFrom); err != nil {
			return "", err
		}
	}
	fingerprint := Hash(Encode(struct {
		Repo, Author string
		Request      CommitRequest
	}{repo, p.Name, r}))
	requestKey := p.Name + "/" + r.RequestID
	if old, err := e.Meta.Request(ctx, repo, requestKey, fingerprint); err != nil || old != "" {
		return old, err
	}
	parentRoot, err := e.Root(ctx, repo, r.Parents[0])
	if err != nil {
		return "", err
	}
	for filename, v := range r.Changes {
		old, err := e.Lookup(ctx, parentRoot, filename)
		if err != nil {
			return "", err
		}
		if v == nil && (old == nil || old.Kind != "file") {
			return "", fail(400, "deletion must name an existing file", filename)
		}
	}
	root, err := e.apply(ctx, parentRoot, r.Changes)
	if err != nil {
		return "", err
	}
	c := Changeset{Version: 1, Parents: r.Parents, Changes: r.Changes, Author: p.Name, Message: r.Message, TimeNS: time.Now().UnixNano(), RebasedFrom: r.RebasedFrom}
	if r.OriginalGit != nil {
		seconds, _, _ := strings.Cut(r.OriginalGit.CommitterTime, " ")
		stamp, err := strconv.ParseInt(seconds, 10, 64)
		if err != nil || stamp > 9223372036 || stamp < -9223372036 {
			return "", fail(400, "invalid imported timestamp")
		}
		c.TimeNS = stamp * 1e9
		c.OriginalGit = r.OriginalGit
	}
	id, err := PutJSON(ctx, e.Objects, "changeset", c)
	if err != nil {
		return "", err
	}
	derived := map[string]string{ManifestKind: root}
	for _, kind := range []string{GraphKind, HistoryKind} {
		obj, err := e.Deriver.Build(ctx, repo, id, c, kind)
		if err != nil {
			return "", err
		}
		derived[kind] = obj
	}
	return e.Meta.Publish(ctx, repo, id, requestKey, fingerprint, derived)
}

func (e *Engine) Diff(ctx context.Context, repo, left, right string, p Principal) (map[string]Difference, error) {
	if _, err := e.Change(ctx, repo, left); err != nil {
		return nil, err
	}
	if _, err := e.Change(ctx, repo, right); err != nil {
		return nil, err
	}
	a, err := e.Root(ctx, repo, left)
	if err != nil {
		return nil, err
	}
	b, err := e.Root(ctx, repo, right)
	if err != nil {
		return nil, err
	}
	result := map[string]Difference{}
	err = e.diffTrees(ctx, a, b, "", result)
	if err != nil {
		return nil, err
	}
	for filename := range result {
		if err = p.Check(filename, false); err != nil {
			return nil, fail(403, "comparison includes paths outside your read permissions")
		}
	}
	return result, nil
}
func (e *Engine) diffTrees(ctx context.Context, left, right, prefix string, out map[string]Difference) error {
	if left == right {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return diffEntries(ctx, e.Objects, left, right, func(n string, x, y *Entry) error {
		xok, yok := x != nil, y != nil
		filename := prefix + n
		xTree, yTree := xok && x.Kind == "tree", yok && y.Kind == "tree"
		var old, new *File
		if xok && !xTree {
			old = &File{ID: x.ID, Mode: x.Mode, Size: x.Size}
		}
		if yok && !yTree {
			new = &File{ID: y.ID, Mode: y.Mode, Size: y.Size}
		}
		if old != nil || new != nil {
			out[filename] = Difference{old, new}
			if len(out) > MaxChanges {
				return fail(413, "diff exceeds changed-path limit")
			}
		}
		if xTree || yTree {
			aa, bb := "", ""
			if xTree {
				aa = x.ID
			}
			if yTree {
				bb = y.ID
			}
			if err := e.diffTrees(ctx, aa, bb, filename+"/", out); err != nil {
				return err
			}
		}
		return nil
	})
}
func (e *Engine) History(ctx context.Context, repo, revision, filename, cursor string, limit int, p Principal) ([]HistoryNode, string, error) {
	if err := ValidPath(filename, false); err != nil {
		return nil, "", err
	}
	if err := p.Check(filename, false); err != nil {
		return nil, "", err
	}
	if _, err := e.Change(ctx, repo, revision); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 1000 {
		return nil, "", fail(400, "history limit must be 1..1000")
	}
	root, err := e.Deriver.Get(ctx, repo, revision, HistoryKind)
	if err != nil {
		return nil, "", err
	}
	entry, err := e.lookup(ctx, root, historyPath(filename))
	if err != nil || entry == nil {
		return []HistoryNode{}, "", err
	}
	id := entry.ID
	// A cursor is accepted only if it belongs to this authorized file's history chain.
	seeking := cursor != ""
	result := []HistoryNode{}
	for id != "" {
		if err = ctx.Err(); err != nil {
			return nil, "", err
		}
		var node HistoryNode
		if err = GetJSON(ctx, e.Objects, "history", id, &node); err != nil {
			return nil, "", err
		}
		if seeking {
			if id == cursor {
				seeking = false
			} else {
				id = node.Previous
				continue
			}
		}
		result = append(result, node)
		id = node.Previous
		if len(result) == limit {
			return result, id, nil
		}
	}
	if seeking {
		return nil, "", fail(400, "invalid history cursor")
	}
	return result, "", nil
}
func (e *Engine) Resolve(ctx context.Context, repo, revision string) (string, error) {
	if idPattern.MatchString(revision) {
		_, err := e.Change(ctx, repo, revision)
		return revision, err
	}
	if err := ValidName(revision); err != nil {
		return "", err
	}
	return e.Meta.Ref(ctx, repo, revision)
}
func (e *Engine) describe(err error, operation string) error {
	if err == nil {
		return nil
	}
	if statusOf(err) != 500 {
		return err
	}
	return fmt.Errorf("%s: %w", operation, err)
}
