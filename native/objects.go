package boughline

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Every tier uses exactly these packed bytes on disk and on the wire.
func Pack(raw []byte) ([]byte, error) {
	if len(raw) > MaxBlob {
		return nil, fail(413, "object too large")
	}
	var buf bytes.Buffer
	buf.WriteString("BG01")
	_ = binary.Write(&buf, binary.BigEndian, uint64(len(raw)))
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(raw); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
func Unpack(packed []byte, id string) ([]byte, error) {
	if len(packed) < 12 || string(packed[:4]) != "BG01" {
		return nil, fail(502, "invalid object envelope")
	}
	n := binary.BigEndian.Uint64(packed[4:12])
	if n > MaxBlob {
		return nil, fail(413, "object too large")
	}
	r, err := gzip.NewReader(bytes.NewReader(packed[12:]))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, int64(n)+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(raw)) != n || Hash(raw) != id {
		return nil, fail(502, "object identity mismatch")
	}
	return raw, nil
}

type ObjectStore interface {
	Get(context.Context, string, string) ([]byte, error)
	Put(context.Context, string, string, []byte) error
}

type S3Store struct {
	Client *minio.Client
	Bucket string
}

func OpenS3(endpoint, bucket, key, secret string, tls bool) (*S3Store, error) {
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: tls})
	if err != nil {
		return nil, err
	}
	return &S3Store{Client: c, Bucket: bucket}, nil
}
func (s *S3Store) Get(ctx context.Context, kind, id string) ([]byte, error) {
	key, err := objectKey(kind, id)
	if err != nil {
		return nil, err
	}
	o, err := s.Client.GetObject(ctx, s.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	data, err := io.ReadAll(io.LimitReader(o, MaxBlob+1<<20))
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fail(404, "object not found")
		}
		return nil, err
	}
	if len(data) > MaxBlob+1<<19 {
		return nil, fail(413, "packed object too large")
	}
	return data, nil
}
func (s *S3Store) Put(ctx context.Context, kind, id string, packed []byte) error {
	key, err := objectKey(kind, id)
	if err != nil {
		return err
	}
	if _, err = Unpack(packed, id); err != nil {
		return err
	}
	// Deterministic packing plus a content-addressed key makes concurrent writes identical.
	_, err = s.Client.PutObject(ctx, s.Bucket, key, bytes.NewReader(packed), int64(len(packed)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

// DiskObjects is a disposable daemon/cache store, and a hermetic test backend.
// Authoritative production servers use S3Store.
type DiskObjects struct {
	Directory   string
	Limit       int64
	mu          sync.Mutex
	index       map[string]*diskEntry
	order       diskHeap
	bytes       int64
	generation  string
	initialized bool
}

func syncDir(name string) error {
	f, e := os.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func AtomicFile(name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(filepath.Dir(name))
}
func (d *DiskObjects) Get(ctx context.Context, kind, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := objectKey(kind, id)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(d.Directory, key)
	d.mu.Lock()
	defer d.mu.Unlock()
	file, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(404, "object not found")
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, MaxBlob+(1<<20)+1))
	if len(b) > MaxBlob+(1<<20) {
		return nil, fail(502, "cached object exceeds limit")
	}
	return b, err
}
func (d *DiskObjects) Put(ctx context.Context, kind, id string, packed []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := objectKey(kind, id)
	if err != nil {
		return err
	}
	if _, err = Unpack(packed, id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Limit > 0 && int64(len(packed)) > d.Limit {
		return nil
	}
	unlock, err := d.prepareDisk()
	if err != nil {
		return err
	}
	defer unlock()

	p := filepath.Join(d.Directory, key)
	if old, e := os.ReadFile(p); e == nil {
		if _, e = Unpack(old, id); e == nil {
			return nil
		}
	}
	if err = AtomicFile(p, packed, 0600); err != nil {
		return err
	}
	return d.recordDisk(p, int64(len(packed)))
}

type memoryObject struct {
	key  string
	data []byte
}
type CachedObjects struct {
	Backend ObjectStore
	Limit   int64
	mu      sync.Mutex
	items   map[string]*list.Element
	order   *list.List
	size    int64
}

func NewCachedObjects(backend ObjectStore, limit int64) *CachedObjects {
	return &CachedObjects{Backend: backend, Limit: limit, items: map[string]*list.Element{}, order: list.New()}
}
func (c *CachedObjects) Get(ctx context.Context, kind, id string) ([]byte, error) {
	key, err := objectKey(kind, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if e := c.items[key]; e != nil {
		c.order.MoveToFront(e)
		data := e.Value.(memoryObject).data
		c.mu.Unlock()
		return data, nil
	}
	c.mu.Unlock()
	b, err := c.Backend.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if _, err = Unpack(b, id); err != nil {
		return nil, err
	}
	c.remember(key, b)
	return b, nil
}
func (c *CachedObjects) Put(ctx context.Context, kind, id string, b []byte) error {
	key, err := objectKey(kind, id)
	if err != nil {
		return err
	}
	if err = c.Backend.Put(ctx, kind, id, b); err != nil {
		return err
	}
	c.remember(key, b)
	return nil
}
func (c *CachedObjects) remember(key string, b []byte) {
	if int64(len(b)) > c.Limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.items[key]; e != nil {
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(memoryObject{key, b})
	c.size += int64(len(b))
	for c.size > c.Limit {
		e := c.order.Back()
		v := e.Value.(memoryObject)
		c.size -= int64(len(v.data))
		delete(c.items, v.key)
		c.order.Remove(e)
	}
}
func PutRaw(ctx context.Context, s ObjectStore, kind string, raw []byte) (string, error) {
	id := Hash(raw)
	p, err := Pack(raw)
	if err != nil {
		return "", err
	}
	return id, s.Put(ctx, kind, id, p)
}
func GetRaw(ctx context.Context, s ObjectStore, kind, id string) ([]byte, error) {
	p, err := s.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	return Unpack(p, id)
}
func PutJSON(ctx context.Context, s ObjectStore, kind string, v any) (string, error) {
	return PutRaw(ctx, s, kind, Encode(v))
}
func GetJSON(ctx context.Context, s ObjectStore, kind, id string, v any) error {
	b, err := GetRaw(ctx, s, kind, id)
	if err != nil {
		return err
	}
	if err = jsonDecode(b, v); err != nil {
		return fmt.Errorf("corrupt %s: %w", kind, err)
	}
	return nil
}
