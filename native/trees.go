package boughline

import (
	"context"
	"strings"
)

const treePageSize = 128

func nodeTree(ctx context.Context, s ObjectStore, id string) (Tree, error) {
	var t Tree
	err := GetJSON(ctx, s, "tree", id, &t)
	return t, err
}
func sharded(t Tree) bool {
	for _, entry := range t {
		return entry.Kind == "shard"
	}
	return false
}
func statistics(t Tree) (items, count int64) {
	for _, entry := range t {
		if entry.Kind == "shard" {
			items += entry.Items
			count += entry.Count
		} else {
			items++
			if entry.Kind == "tree" {
				count += entry.Count
			} else {
				count++
			}
		}
	}
	return
}
func treeStats(ctx context.Context, s ObjectStore, id string) (int64, int64, error) {
	t, err := nodeTree(ctx, s, id)
	if err != nil {
		return 0, 0, err
	}
	items, count := statistics(t)
	return items, count, nil
}
func LookupEntry(ctx context.Context, s ObjectStore, id, name string) (*Entry, error) {
	hash := Hash([]byte(name))
	for depth := 0; depth <= len(hash); depth++ {
		t, err := nodeTree(ctx, s, id)
		if err != nil {
			return nil, err
		}
		if !sharded(t) {
			entry, ok := t[name]
			if !ok {
				return nil, nil
			}
			return &entry, nil
		}
		if depth >= len(hash) {
			return nil, fail(500, "invalid directory trie")
		}
		entry, ok := t[":"+hash[depth:depth+1]]
		if !ok {
			return nil, nil
		}
		id = entry.ID
	}
	return nil, fail(500, "invalid directory trie")
}
func WalkEntries(ctx context.Context, s ObjectStore, id string, visit func(string, Entry) error) error {
	t, err := nodeTree(ctx, s, id)
	if err != nil {
		return err
	}
	for name, entry := range t {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Kind == "shard" {
			if err = WalkEntries(ctx, s, entry.ID, visit); err != nil {
				return err
			}
		} else {
			if err = visit(name, entry); err != nil {
				return err
			}
		}
	}
	return nil
}
func flatTree(ctx context.Context, s ObjectStore, id string) (Tree, error) {
	out := Tree{}
	err := WalkEntries(ctx, s, id, func(name string, entry Entry) error { out[name] = entry; return nil })
	return out, err
}
func buildPage(ctx context.Context, s ObjectStore, t Tree, depth int) (string, error) {
	if len(t) <= treePageSize {
		return PutJSON(ctx, s, "tree", t)
	}
	if depth >= 64 {
		return "", fail(500, "directory hash collision limit")
	}
	groups := map[string]Tree{}
	for name, entry := range t {
		nibble := Hash([]byte(name))[depth : depth+1]
		if groups[nibble] == nil {
			groups[nibble] = Tree{}
		}
		groups[nibble][name] = entry
	}
	branches := Tree{}
	for nibble, entries := range groups {
		id, err := buildPage(ctx, s, entries, depth+1)
		if err != nil {
			return "", err
		}
		items, count, err := treeStats(ctx, s, id)
		if err != nil {
			return "", err
		}
		branches[":"+nibble] = Entry{Kind: "shard", ID: id, Items: items, Count: count}
	}
	return PutJSON(ctx, s, "tree", branches)
}
func setEntry(ctx context.Context, s ObjectStore, root, name string, value *Entry, depth int) (string, error) {
	t, err := nodeTree(ctx, s, root)
	if err != nil {
		return "", err
	}
	if !sharded(t) {
		if value == nil {
			delete(t, name)
		} else {
			t[name] = *value
		}
		return buildPage(ctx, s, t, depth)
	}
	if depth >= 64 {
		return "", fail(500, "invalid directory trie")
	}
	nibble := Hash([]byte(name))[depth : depth+1]
	slot := ":" + nibble
	child, exists := t[slot]
	if !exists {
		if value == nil {
			return root, nil
		}
		id, err := PutJSON(ctx, s, "tree", Tree{})
		if err != nil {
			return "", err
		}
		child = Entry{ID: id}
	}
	newID, err := setEntry(ctx, s, child.ID, name, value, depth+1)
	if err != nil {
		return "", err
	}
	items, count, err := treeStats(ctx, s, newID)
	if err != nil {
		return "", err
	}
	if items == 0 {
		delete(t, slot)
	} else {
		t[slot] = Entry{Kind: "shard", ID: newID, Items: items, Count: count}
	}
	total, _ := statistics(t)
	if total <= treePageSize {
		flat := Tree{}
		for _, branch := range t {
			if err = WalkEntries(ctx, s, branch.ID, func(n string, e Entry) error { flat[n] = e; return nil }); err != nil {
				return "", err
			}
		}
		return PutJSON(ctx, s, "tree", flat)
	}
	return PutJSON(ctx, s, "tree", t)
}
func patchEntries(ctx context.Context, s ObjectStore, root string, updates map[string]*Entry) (string, error) {
	names := []string{}
	for name := range updates {
		names = append(names, name)
	}
	sortStrings(names)
	var err error
	for _, name := range names {
		root, err = setEntry(ctx, s, root, name, updates[name], 0)
		if err != nil {
			return "", err
		}
	}
	return root, nil
}
func diffEntries(ctx context.Context, s ObjectStore, a, b string, visit func(string, *Entry, *Entry) error) error {
	if a == b {
		return nil
	}
	left, right := Tree{}, Tree{}
	var err error
	if a != "" {
		left, err = nodeTree(ctx, s, a)
		if err != nil {
			return err
		}
	}
	if b != "" {
		right, err = nodeTree(ctx, s, b)
		if err != nil {
			return err
		}
	}
	if sharded(left) && sharded(right) {
		keys := map[string]bool{}
		for k := range left {
			keys[k] = true
		}
		for k := range right {
			keys[k] = true
		}
		for k := range keys {
			if err = diffEntries(ctx, s, left[k].ID, right[k].ID, visit); err != nil {
				return err
			}
		}
		return nil
	}
	if sharded(left) {
		remaining := right
		err = WalkEntries(ctx, s, a, func(name string, entry Entry) error {
			other, ok := remaining[name]
			delete(remaining, name)
			if ok {
				if entry == other {
					return nil
				}
				return visit(name, &entry, &other)
			}
			return visit(name, &entry, nil)
		})
		if err != nil {
			return err
		}
		for name, entry := range remaining {
			if err = visit(name, nil, &entry); err != nil {
				return err
			}
		}
		return nil
	}
	if sharded(right) {
		return diffEntries(ctx, s, b, a, func(name string, x, y *Entry) error { return visit(name, y, x) })
	}
	keys := map[string]bool{}
	for k := range left {
		keys[k] = true
	}
	for k := range right {
		keys[k] = true
	}
	for name := range keys {
		x, xok := left[name]
		y, yok := right[name]
		var xp, yp *Entry
		if xok {
			xp = &x
		}
		if yok {
			yp = &y
		}
		if xok && yok && x == y {
			continue
		}
		if err = visit(name, xp, yp); err != nil {
			return err
		}
	}
	return nil
}
func PageID(ctx context.Context, s ObjectStore, root, page string) (string, error) {
	if len(page) > 64 || strings.IndexFunc(page, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 {
		return "", fail(400, "invalid tree page")
	}
	id := root
	for _, r := range page {
		t, err := nodeTree(ctx, s, id)
		if err != nil {
			return "", err
		}
		branch, ok := t[":"+string(r)]
		if !ok || branch.Kind != "shard" {
			return "", fail(403, "tree page is outside the authorized snapshot")
		}
		id = branch.ID
	}
	return id, nil
}
