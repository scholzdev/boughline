package boughline

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPagedTreesCanonicalAndIncremental(t *testing.T) {
	f := setup(t)
	tree := Tree{}
	for i := 0; i < 4096; i++ {
		tree[fmt.Sprintf("file-%04d", i)] = Entry{Kind: "file", ID: Hash([]byte(fmt.Sprint(i))), Mode: Regular}
	}
	root, err := buildPage(f.ctx, f.e.Objects, tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	physical, _ := nodeTree(f.ctx, f.e.Objects, root)
	if len(physical) > 16 || !sharded(physical) {
		t.Fatal("directory not paged")
	}
	replacement := Entry{Kind: "file", ID: Hash([]byte("replacement")), Mode: Regular}
	updated, err := patchEntries(f.ctx, f.e.Objects, root, map[string]*Entry{"file-0001": &replacement})
	if err != nil {
		t.Fatal(err)
	}
	tree["file-0001"] = replacement
	expected, _ := buildPage(f.ctx, f.e.Objects, tree, 0)
	if expected != updated {
		t.Fatal("page identities depend on mutation order")
	}
	pages, _ := nodeTree(f.ctx, f.e.Objects, updated)
	changed := 0
	for name, entry := range physical {
		if entry != pages[name] {
			changed++
		}
	}
	if changed != 1 {
		t.Fatal("unrelated pages rewritten", changed)
	}
	diffs := 0
	if err = diffEntries(f.ctx, f.e.Objects, root, updated, func(name string, a, b *Entry) error {
		diffs++
		if name != "file-0001" {
			t.Fatal(name)
		}
		return nil
	}); err != nil || diffs != 1 {
		t.Fatal(err, diffs)
	}
	small := Tree{}
	for i := 0; i < 129; i++ {
		name := fmt.Sprintf("f%d", i)
		small[name] = replacement
	}
	smallRoot, err := buildPage(f.ctx, f.e.Objects, small, 0)
	if err != nil {
		t.Fatal(err)
	}
	delete(small, "f0")
	shrunk, err := patchEntries(f.ctx, f.e.Objects, smallRoot, map[string]*Entry{"f0": nil})
	if err != nil {
		t.Fatal(err)
	}
	expected, _ = buildPage(f.ctx, f.e.Objects, small, 0)

	if shrunk != expected {
		t.Fatal("directory did not canonically coalesce")
	}
}

func TestPagedSparseAndIAM(t *testing.T) {
	f := setup(t)
	changes := map[string]*File{}
	for i := 0; i < 512; i++ {
		changes[fmt.Sprintf("dir/f%04d", i)] = f.file(t, "data")
	}
	changes["private/secret"] = f.file(t, "secret")
	revision := f.commit(t, f.genesis, changes)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	principal := Principal{Name: "limited", Rules: []Rule{{Prefix: "dir/f0001", Read: true}}}
	checkout, err := f.e.Checkout(f.ctx, "test", revision, principal, key)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := f.e.Tree(f.ctx, checkout.Root)
	if len(root) != 1 {
		t.Fatal("leaked top-level names", root)
	}
	dir, _ := f.e.Tree(f.ctx, root["dir"].ID)
	if len(dir) != 1 || dir["f0001"].ID == "" {
		t.Fatal("leaked denied names", dir)
	}
	full, err := f.e.Checkout(f.ctx, "test", revision, f.p, key)
	if err != nil {
		t.Fatal(err)
	}
	service := NewObjectService("daemon", f.e.Objects, pub, "")
	client := &Client{Local: &DiskObjects{Directory: t.TempDir()}}
	client.Daemon = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		service.Handler(recorder, r)
		return recorder.Result(), nil
	})}
	files, err := client.Manifest(f.ctx, full, []string{"dir/f0001"})
	if err != nil || len(files) != 1 {
		t.Fatal(err, len(files))
	}
	if service.Metrics.Requests.Load() > 4 {
		t.Fatal("sparse checkout fetched unrelated pages", service.Metrics.Requests.Load())
	}
	cap, _ := VerifyCapability(full.Capability, pub, time.Now())
	directory, _ := f.e.Lookup(f.ctx, full.Root, "dir")
	physical, _ := nodeTree(f.ctx, f.e.Objects, directory.ID)
	for slot, entry := range physical {
		if err = service.authorize(f.ctx, cap, ObjectRequest{Kind: "tree", Path: "dir", ID: entry.ID, Page: slot[1:]}); err != nil {
			t.Fatal(err)
		}
		requireError(t, service.authorize(f.ctx, cap, ObjectRequest{Kind: "tree", Path: "", ID: entry.ID, Page: slot[1:]}), 403)
		break
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDisplacementPreservesRacingEditsAndUnknownFiles(t *testing.T) {
	root := t.TempDir()
	w := &Workspace{Root: root}
	data := []byte("old")
	old := File{ID: Hash(data), Mode: Regular, Size: 3}
	if err := WriteLocal(root, "tracked", old, data); err != nil {
		t.Fatal(err)
	}
	open, err := os.OpenFile(filepath.Join(root, "tracked"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if err = os.WriteFile(filepath.Join(root, "new"), []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, ".bough/backups/op/displaced")
	if err = w.displace([]string{"tracked", "new"}, map[string]File{"tracked": old}, backup); err != nil {
		t.Fatal(err)
	}
	if _, err = open.WriteAt([]byte("edit"), 0); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(backup, "tracked"))
	if string(raw) != "edit" {
		t.Fatal("open editor data lost")
	}
	raw, _ = os.ReadFile(filepath.Join(root, "new"))
	if string(raw) != "unknown" {
		t.Fatal("unknown file removed")
	}
	requireError(t, WriteLocal(root, "new", old, data), 409)
}

func TestRecoveryChecksAllBackupsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	w := &Workspace{Root: root}
	oldData, newData := []byte("old"), []byte("new")
	old := File{ID: Hash(oldData), Mode: Regular, Size: 3}
	new := File{ID: Hash(newData), Mode: Regular, Size: 3}
	op := RequestID()
	before := WorkspaceState{Version: 1, Files: map[string]File{"a": old, "b": old}}
	after := WorkspaceState{Version: 1, Files: map[string]File{"a": new, "b": new}}
	for _, p := range []string{"a", "b"} {
		if err := WriteLocal(root, p, new, newData); err != nil {
			t.Fatal(err)
		}
	}
	backup := filepath.Join(root, ".bough/backups", op, "files")
	if err := WriteLocal(backup, "a", old, oldData); err != nil {
		t.Fatal(err)
	}
	if err := AtomicFile(filepath.Join(root, ".bough/recovery.json"), Encode(recovery{Operation: op, Before: before, After: after, Paths: []string{"a", "b"}}), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := w.Recover()
	requireError(t, err, 500)
	a, _, _ := ReadLocal(root, "a")
	if a == nil || *a != new {
		t.Fatal("mutated before checking missing backup")
	}
	if err = WriteLocal(backup, "b", old, oldData); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Recover(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		actual, _, _ := ReadLocal(root, p)
		if actual == nil || *actual != old {
			t.Fatal("restore failed", p)
		}
	}
}

func TestWorkspaceAliasesShareMonitorIdentity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, ".bough"), 0700); err != nil {
		t.Fatal(err)
	}
	state := WorkspaceState{Version: 1, Server: "http://localhost", Repo: "test", Files: map[string]File{}, Prefixes: []string{""}}
	if err := AtomicFile(filepath.Join(root, ".bough/state.json"), Encode(state), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	one, err := OpenWorkspace(root, "01234567890123456789012345678901", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := OpenWorkspace(filepath.Join(alias, "project"), "01234567890123456789012345678901", "")
	if err != nil {
		t.Fatal(err)
	}
	if one.monitorSocket() != two.monitorSocket() {
		t.Fatal("aliased directories use different monitor sockets")
	}
}

func TestDirectoryPruningCannotUnlinkRacingFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "replaced"), []byte("unknown concurrent file"), 0600); err != nil {
		t.Fatal(err)
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err = removeEmptyDirectory(directory, "replaced"); err == nil {
		t.Fatal("directory pruning removed a regular file")
	}
	raw, err := os.ReadFile(filepath.Join(root, "replaced"))
	if err != nil || string(raw) != "unknown concurrent file" {
		t.Fatal("racing file was lost", err)
	}
}
