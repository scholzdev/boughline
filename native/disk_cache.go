package boughline

import (
	"container/heap"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// One bounded FIFO index replaces a full directory walk for each cache write.
// A generation plus a filesystem lock keeps independent CLI processes coherent;
// the normal node/cache daemon owns its directory and scans only on startup.
const maxDiskObjects = 100000

type diskEntry struct {
	Path      string
	Size, Age int64
	Index     int
}
type diskHeap []*diskEntry

func (h diskHeap) Len() int           { return len(h) }
func (h diskHeap) Less(i, j int) bool { return h[i].Age < h[j].Age }
func (h diskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].Index = i; h[j].Index = j }
func (h *diskHeap) Push(v any) {
	entry := v.(*diskEntry)
	entry.Index = len(*h)
	*h = append(*h, entry)
}
func (h *diskHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	*h = old[:len(old)-1]
	return entry
}
func (d *DiskObjects) evictDisk() error {
	for d.bytes > d.Limit || len(d.order) > maxDiskObjects {
		entry := heap.Pop(&d.order).(*diskEntry)
		delete(d.index, entry.Path)
		d.bytes -= entry.Size
		if err := os.Remove(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			d.initialized = false
			return err
		}
	}
	return nil
}
func (d *DiskObjects) prepareDisk() (func(), error) {
	if d.Limit <= 0 {
		return func() {}, nil
	}
	if err := os.MkdirAll(d.Directory, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(d.Directory, ".cache.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		return nil, err
	}
	unlock := func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }
	raw, err := os.ReadFile(filepath.Join(d.Directory, ".cache.generation"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, err
	}
	if d.initialized && string(raw) == d.generation {
		return unlock, nil
	}
	d.index = map[string]*diskEntry{}
	d.order = diskHeap{}
	d.bytes = 0
	err = filepath.Walk(d.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".tmp-") {
			return os.Remove(path)
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		entry := &diskEntry{Path: path, Size: info.Size(), Age: info.ModTime().UnixNano()}
		d.index[path] = entry
		heap.Push(&d.order, entry)
		d.bytes += entry.Size
		return d.evictDisk()
	})
	if err != nil {
		d.initialized = false
		unlock()
		return nil, err
	}
	d.initialized = true
	// Eviction is itself a mutation visible to other processes.
	d.generation = RequestID()
	if err = os.WriteFile(filepath.Join(d.Directory, ".cache.generation"), []byte(d.generation), 0600); err != nil {
		d.initialized = false
		unlock()
		return nil, err
	}
	return unlock, nil
}
func (d *DiskObjects) recordDisk(path string, size int64) error {
	if d.Limit <= 0 {
		return nil
	}
	if old := d.index[path]; old != nil {
		heap.Remove(&d.order, old.Index)
		delete(d.index, path)
		d.bytes -= old.Size
	}
	entry := &diskEntry{Path: path, Size: size, Age: time.Now().UnixNano()}
	heap.Push(&d.order, entry)
	d.index[path] = entry
	d.bytes += size
	if err := d.evictDisk(); err != nil {
		return err
	}
	d.generation = RequestID()
	if err := os.WriteFile(filepath.Join(d.Directory, ".cache.generation"), []byte(d.generation), 0600); err != nil {
		d.initialized = false
		return err
	}
	return nil
}
