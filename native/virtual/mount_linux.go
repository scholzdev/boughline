//go:build linux

package virtual

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	bough "github.com/scholzdev/boughline/native"
)

// CI/research snapshots are read-only. Editable sparse workspaces use the same
// object protocol and the normal checkout path; no Git data is fetched in either.
type node struct {
	fs.Inode
	client                *bough.Client
	checkout              bough.Checkout
	prefixes              []string
	path, id, kind, cache string
	mode                  uint32
	size                  uint64
}

func (n *node) visible(path string) bool {
	if bough.Selected(path, n.prefixes) {
		return true
	}
	for _, p := range n.prefixes {
		if strings.HasPrefix(p, path+"/") {
			return true
		}
	}
	return false
}
func (n *node) entries(ctx context.Context) (bough.Tree, error) {
	return n.client.Directory(ctx, n.checkout, n.id, n.path, n.prefixes)
}

func (n *node) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = n.mode
	out.Size = n.size
	out.Nlink = 1
	return 0
}
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	entry, err := n.client.DirectoryEntry(ctx, n.checkout, n.id, n.path, name)
	if err != nil {
		return nil, syscall.EIO
	}
	path := name
	if n.path != "" {
		path = n.path + "/" + name
	}
	if entry == nil || !n.visible(path) {
		return nil, syscall.ENOENT
	}
	mode := entry.Mode
	if entry.Kind == "tree" {
		mode = syscall.S_IFDIR | 0555
	}
	child := &node{client: n.client, checkout: n.checkout, prefixes: n.prefixes, path: path, id: entry.ID, kind: entry.Kind, cache: n.cache, mode: mode, size: uint64(entry.Size)}
	out.Mode = mode
	out.Size = uint64(entry.Size)
	out.Nlink = 1
	return n.NewInode(ctx, child, fs.StableAttr{Mode: mode & syscall.S_IFMT}), 0
}
func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	tree, err := n.entries(ctx)
	if err != nil {
		return nil, syscall.EIO
	}
	entries := []fuse.DirEntry{}
	for name, entry := range tree {
		path := name
		if n.path != "" {
			path = n.path + "/" + name
		}
		if !n.visible(path) {
			continue
		}
		mode := entry.Mode
		if entry.Kind == "tree" {
			mode = syscall.S_IFDIR
		}
		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}
func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	if n.mode&syscall.S_IFMT != syscall.S_IFLNK {
		return nil, syscall.EINVAL
	}
	data, err := n.client.Object(ctx, n.checkout, "blob", n.id, n.path)
	if err != nil {
		return nil, syscall.EIO
	}
	return data, 0
}

type handle struct {
	n    *node
	mu   sync.Mutex
	file *os.File
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	return &handle{n: n}, fuse.FOPEN_KEEP_CACHE, 0
}
func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		data, err := h.n.client.Object(ctx, h.n.checkout, "blob", h.n.id, h.n.path)
		if err != nil {
			return nil, syscall.EIO
		}
		if err = os.MkdirAll(h.n.cache, 0700); err != nil {
			return nil, syscall.EIO
		}
		temporary, err := os.CreateTemp(h.n.cache, ".hydrate-")
		if err != nil {
			return nil, syscall.EIO
		}
		name := temporary.Name()
		_ = os.Remove(name)
		if _, err = temporary.Write(data); err != nil {
			_ = temporary.Close()
			return nil, syscall.EIO
		}
		h.file = temporary
		if err != nil {
			return nil, syscall.EIO
		}
	}
	return fuse.ReadResultFd(h.file.Fd(), off, len(dest)), 0
}
func (h *handle) Release(context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		_ = h.file.Close()
	}
	return 0
}
func Mount(ctx context.Context, c *bough.Client, revision string, prefixes []string, mountpoint, cache string) error {
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	for _, p := range prefixes {
		if err := bough.ValidPath(p, true); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(mountpoint, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(mountpoint)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("virtual checkout mountpoint must be empty")
	}
	checkout, err := c.Checkout(ctx, revision)
	if err != nil {
		return err
	}
	root := &node{client: c, checkout: checkout, prefixes: prefixes, id: checkout.Root, kind: "tree", cache: cache, mode: syscall.S_IFDIR | 0555}
	ttl := time.Second
	server, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}, FsName: "boughline:" + c.Repo, Name: "boughline", MaxBackground: 16}, EntryTimeout: &ttl, AttrTimeout: &ttl})
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = server.Unmount() }()
	server.Wait()
	return nil
}
