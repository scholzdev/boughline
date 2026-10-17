package boughline

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func CreateSnapshot(ctx context.Context, c *Client, revision string, prefixes []string, destination string) error {
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	if _, err := os.Lstat(destination); err == nil {
		return fail(409, "snapshot destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".snapshot-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	checkout, err := c.Checkout(ctx, revision)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	add := func(kind, id string, data []byte) error {
		key := kind + "/" + id
		if seen[key] {
			return nil
		}
		seen[key] = true
		packed, err := Pack(data)
		if err != nil {
			return err
		}
		if err = tw.WriteHeader(&tar.Header{Name: key, Mode: 0600, Size: int64(len(packed)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(packed)
		return err
	}
	var walk func(string, string) error
	walk = func(id, prefix string) error {
		return c.walkDirectory(ctx, checkout, id, prefix, prefixes, func(id string, raw []byte) error { return add("tree", id, raw) }, func(name string, entry Entry) error {
			p := name
			if prefix != "" {
				p = prefix + "/" + name
			}
			relevant := Selected(p, prefixes)
			for _, want := range prefixes {
				if under(want, p) {
					relevant = true
				}
			}
			if !relevant {
				return nil
			}
			if entry.Kind == "tree" {
				if err := walk(entry.ID, p); err != nil {
					return err
				}
			} else {
				data, err := c.Object(ctx, checkout, "blob", entry.ID, p)
				if err != nil {
					return err
				}
				if err = add("blob", entry.ID, data); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err = walk(checkout.Root, ""); err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(temp, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}
func WarmSnapshot(ctx context.Context, store ObjectStore, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := tar.NewReader(f)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > MaxBlob+(1<<20) {
			return fail(400, "invalid snapshot entry")
		}
		kind, id, ok := strings.Cut(header.Name, "/")
		if !ok || (kind != "tree" && kind != "blob") || ValidID(id) != nil {
			return fail(400, "invalid snapshot object name")
		}
		packed, err := io.ReadAll(io.LimitReader(reader, header.Size))
		if err != nil {
			return err
		}
		if int64(len(packed)) != header.Size {
			return fail(400, "truncated snapshot")
		}
		if err = store.Put(ctx, kind, id, packed); err != nil {
			return err
		}
	}
}
