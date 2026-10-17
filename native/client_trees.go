package boughline

import (
	"context"
	"strings"
)

// walkDirectory fetches only pages relevant to the sparse view. The filesystem
// path and the physical hash-trie page remain separate authorization coordinates.
func (c *Client) walkDirectory(ctx context.Context, checkout Checkout, id, path string, prefixes []string, pageVisit func(string, []byte) error, visit func(string, Entry) error) error {
	var walk func(string, string) error
	walk = func(id, page string) error {
		raw, err := c.objectPage(ctx, checkout, "tree", id, path, page)
		if err != nil {
			return err
		}
		if pageVisit != nil {
			if err = pageVisit(id, raw); err != nil {
				return err
			}
		}
		var tree Tree
		if err = jsonDecode(raw, &tree); err != nil {
			return fail(502, "invalid tree page")
		}
		for name, entry := range tree {
			if entry.Kind == "shard" {
				if len(name) != 2 || name[0] != ':' || !strings.ContainsRune("0123456789abcdef", rune(name[1])) || len(page) >= 64 {
					return fail(502, "invalid tree shard")
				}
				next := page + name[1:]
				relevant := Selected(path, prefixes)
				for _, prefix := range prefixes {
					rest := prefix
					if path != "" {
						if !strings.HasPrefix(rest, path+"/") {
							continue
						}
						rest = strings.TrimPrefix(rest, path+"/")
					}
					child, _, _ := strings.Cut(rest, "/")
					if child != "" && strings.HasPrefix(Hash([]byte(child)), next) {
						relevant = true
					}
				}
				if relevant {
					if err = walk(entry.ID, next); err != nil {
						return err
					}
				}
			} else {
				if strings.Contains(name, "/") || ValidPath(name, false) != nil || (entry.Kind != "tree" && entry.Kind != "file") {
					return fail(502, "invalid tree entry")
				}
				if err = visit(name, entry); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(id, "")
}

func (c *Client) Directory(ctx context.Context, checkout Checkout, id, path string, prefixes []string) (Tree, error) {
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	result := Tree{}
	err := c.walkDirectory(ctx, checkout, id, path, prefixes, nil, func(name string, entry Entry) error { result[name] = entry; return nil })
	return result, err
}

func (c *Client) DirectoryEntry(ctx context.Context, checkout Checkout, id, path, name string) (*Entry, error) {
	if strings.Contains(name, "/") || ValidPath(name, false) != nil {
		return nil, fail(400, "invalid directory entry")
	}
	hash, page := Hash([]byte(name)), ""
	for depth := 0; depth <= 64; depth++ {
		raw, err := c.objectPage(ctx, checkout, "tree", id, path, page)
		if err != nil {
			return nil, err
		}
		var tree Tree
		if err = jsonDecode(raw, &tree); err != nil {
			return nil, err
		}
		if !sharded(tree) {
			entry, ok := tree[name]
			if !ok {
				return nil, nil
			}
			return &entry, nil
		}
		if depth == 64 {
			break
		}
		entry, ok := tree[":"+hash[depth:depth+1]]
		if !ok {
			return nil, nil
		}
		page += hash[depth : depth+1]
		id = entry.ID
	}
	return nil, fail(502, "invalid directory trie")
}
