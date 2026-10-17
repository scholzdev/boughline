package boughline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	e       *Engine
	ctx     context.Context
	p       Principal
	genesis string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{ctx: context.Background(), p: Principal{Name: "owner", Admin: true}}
	f.e = NewEngine(NewMemoryMetadata(), NewCachedObjects(&DiskObjects{Directory: t.TempDir()}, 32<<20))
	var err error
	f.genesis, err = f.e.CreateRepo(f.ctx, RepoConfig{ID: "test", Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) file(t *testing.T, raw string) *File {
	t.Helper()
	id, err := f.e.UploadBlob(f.ctx, "test", f.p, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return &File{ID: id, Mode: Regular, Size: int64(len(raw))}
}
func (f *fixture) commit(t *testing.T, parent string, changes map[string]*File) string {
	t.Helper()
	id, err := f.e.Commit(f.ctx, "test", f.p, CommitRequest{Parents: []string{parent}, Changes: changes, Message: "test", RequestID: fmt.Sprintf("%s-%s", t.Name(), RandomID())})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func RandomID() string { b := make([]byte, 24); _, _ = rand.Read(b); return Hash(b) }
func requireError(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil || statusOf(err) != status {
		t.Fatalf("wanted status %d, got %v", status, err)
	}
}

func TestDeltaTreesAndIndependentPublication(t *testing.T) {
	f := setup(t)
	a := f.commit(t, f.genesis, map[string]*File{"src/a": f.file(t, "a"), "other/b": f.file(t, "b")})
	b := f.commit(t, a, map[string]*File{"src/a": f.file(t, "c")})
	x, _ := f.e.Root(f.ctx, "test", a)
	y, _ := f.e.Root(f.ctx, "test", b)
	xt, _ := f.e.Tree(f.ctx, x)
	yt, _ := f.e.Tree(f.ctx, y)
	if xt["other"] != yt["other"] || xt["src"] == yt["src"] {
		t.Fatal("subtrees not incrementally shared")
	}
	c, _ := f.e.Change(f.ctx, "test", b)
	if len(c.Changes) != 1 {
		t.Fatal("changeset is not a delta")
	}
	head, _ := f.e.Meta.Ref(f.ctx, "test", "main")
	if head != f.genesis {
		t.Fatal("commit moved ref")
	}
	if strings.Contains(string(Encode(c)), `"root"`) {
		t.Fatal("canonical changeset contains derived root")
	}
}
func TestPathHistorySurvivesTypeChanges(t *testing.T) {
	f := setup(t)
	a := f.commit(t, f.genesis, map[string]*File{"a": f.file(t, "one"), "a!history": f.file(t, "x")})
	b := f.commit(t, a, map[string]*File{"a": nil, "a/!history": f.file(t, "nested")})
	c := f.commit(t, b, map[string]*File{"a/!history": nil, "a": f.file(t, "three")})
	nodes, _, err := f.e.History(f.ctx, "test", c, "a", "", 100, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || nodes[0].Commit != c || nodes[1].File != nil || nodes[2].Commit != a {
		t.Fatalf("invalid history: %+v", nodes)
	}
	nested, _, err := f.e.History(f.ctx, "test", c, "a/!history", "", 100, f.p)
	if err != nil || len(nested) != 2 {
		t.Fatalf("nested history lost: %+v %v", nested, err)
	}
}
func TestBlameRecursiveAndEmptyFile(t *testing.T) {
	f := setup(t)
	a := f.commit(t, f.genesis, map[string]*File{"f": f.file(t, "one\ntwo\nthree\n"), "empty": f.file(t, "")})
	b := f.commit(t, a, map[string]*File{"f": f.file(t, "one\nTWO\nthree\n")})
	c := f.commit(t, b, map[string]*File{"unrelated": f.file(t, "x")})
	blame, err := f.e.Blame(f.ctx, "test", c, "f", f.p)
	if err != nil {
		t.Fatal(err)
	}
	if len(blame.Origins) != 3 || blame.Origins[0].Commit != a || blame.Origins[1].Commit != b || blame.Origins[2].Commit != a {
		t.Fatalf("wrong origins: %+v", blame)
	}
	empty, err := f.e.Blame(f.ctx, "test", c, "empty", f.p)
	if err != nil || len(empty.Origins) != 0 {
		t.Fatalf("empty-file blame: %+v %v", empty, err)
	}
}
func TestGraphMergeRebaseAndConflicts(t *testing.T) {
	f := setup(t)
	source := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	a := f.commit(t, f.genesis, map[string]*File{"f": f.file(t, source)})
	left := f.commit(t, a, map[string]*File{"f": f.file(t, strings.ReplaceAll(source, "one", "ONE"))})
	right := f.commit(t, a, map[string]*File{"f": f.file(t, strings.ReplaceAll(source, "seven", "SEVEN"))})
	merged, err := f.e.Merge(f.ctx, "test", left, right, "merge", "merge", f.p)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := f.e.Root(f.ctx, "test", merged)
	entry, _ := f.e.Lookup(f.ctx, root, "f")
	raw, _ := GetRaw(f.ctx, f.e.Objects, "blob", entry.ID)
	if !strings.Contains(string(raw), "ONE") || !strings.Contains(string(raw), "SEVEN") {
		t.Fatal("lost text edits")
	}
	blame, err := f.e.Blame(f.ctx, "test", merged, "f", f.p)
	if err != nil || blame.Origins[0].Commit != left || blame.Origins[6].Commit != right {
		t.Fatal("merge blame lost parent origins", blame, err)
	}
	for _, parent := range []string{left, right} {
		yes, err := f.e.Ancestor(f.ctx, "test", parent, merged)
		if err != nil || !yes {
			t.Fatal("lost parent", err)
		}
	}
	rebased, err := f.e.Rebase(f.ctx, "test", right, left, "rebase", f.p)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := f.e.Change(f.ctx, "test", rebased)
	if len(c.Parents) != 1 || c.Parents[0] != left || c.RebasedFrom != right {
		t.Fatal("invalid rebase")
	}
	conflict := f.commit(t, a, map[string]*File{"f": f.file(t, strings.ReplaceAll(source, "one", "CONFLICT"))})
	_, err = f.e.Merge(f.ctx, "test", left, conflict, "merge", "conflict", f.p)
	requireError(t, err, 409)
}
func TestConcurrentCommitIdempotencyAndCAS(t *testing.T) {
	f := setup(t)
	request := CommitRequest{Parents: []string{f.genesis}, Changes: map[string]*File{"f": f.file(t, "new")}, Message: "same", RequestID: "same"}
	ids := make(chan string, 16)
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); id, err := f.e.Commit(f.ctx, "test", f.p, request); ids <- id; errs <- err }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate logical commits")
		}
	}
	request.Message = "different"
	_, err := f.e.Commit(f.ctx, "test", f.p, request)
	requireError(t, err, 409)
	wins := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); wins <- f.e.Meta.CAS(f.ctx, "test", "main", f.genesis, first) == nil }()
	}
	wg.Wait()
	close(wins)
	count := 0
	for win := range wins {
		if win {
			count++
		}
	}
	if count != 1 {
		t.Fatal("CAS overwrote concurrent writer", count)
	}
}
func TestReviewIAMAndAtomicPolicyGuard(t *testing.T) {
	f := setup(t)
	if err := f.e.Meta.SetPolicies(f.ctx, "test", []ReviewPolicy{{Prefix: "sensitive", Approvals: 1, Reviewers: []string{"reviewer"}}}); err != nil {
		t.Fatal(err)
	}
	revision := f.commit(t, f.genesis, map[string]*File{"sensitive/f": f.file(t, "protected")})
	_, err := f.e.Land(f.ctx, "test", "main", f.genesis, revision, "land", f.p)
	requireError(t, err, 403)
	if err = f.e.Approve(f.ctx, "test", revision, true, f.p); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Land(f.ctx, "test", "main", f.genesis, revision, "land", f.p)
	requireError(t, err, 403)
	reviewer := Principal{Name: "reviewer", Rules: []Rule{{Prefix: "sensitive", Read: true}}}
	if err = f.e.Approve(f.ctx, "test", revision, true, reviewer); err != nil {
		t.Fatal(err)
	}
	guard, err := f.e.checkReview(f.ctx, "test", f.genesis, revision, revision, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.Approve(f.ctx, "test", revision, false, reviewer); err != nil {
		t.Fatal(err)
	}
	requireError(t, f.e.Meta.CAS(f.ctx, "test", "main", f.genesis, revision, guard), 409)
	if err = f.e.Approve(f.ctx, "test", revision, true, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Land(f.ctx, "test", "main", f.genesis, revision, "land", f.p); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Commit(f.ctx, "test", reviewer, CommitRequest{Parents: []string{revision}, Changes: map[string]*File{"sensitive/f": f.file(t, "oops")}, Message: "x", RequestID: "denied"})
	requireError(t, err, 403)
}
func TestProtectedCommentOnlyGoAndDirectives(t *testing.T) {
	f := setup(t)
	a := f.file(t, "package p\n// old\nfunc F() int { return 1 }\n")
	b := f.file(t, "package p\n// changed comment\nfunc F() int { return 1 }\n")
	if !f.e.commentOnly(f.ctx, "x.go", Difference{a, b}) {
		t.Fatal("comment-only Go was not recognized")
	}
	directive := f.file(t, "//go:build windows\n\npackage p\nfunc F() int { return 1 }\n")
	if f.e.commentOnly(f.ctx, "x.go", Difference{a, directive}) {
		t.Fatal("compiler directive bypassed review")
	}
	changed := f.file(t, "package p\nfunc F() int { return 2 }\n")
	if f.e.commentOnly(f.ctx, "x.go", Difference{a, changed}) {
		t.Fatal("logic change exempted")
	}
}
func TestFilteredTreeAndImmutableObjectAuthorization(t *testing.T) {
	f := setup(t)
	revision := f.commit(t, f.genesis, map[string]*File{"public/a": f.file(t, "public"), "private/secret": f.file(t, "secret")})
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	principal := Principal{Name: "reader", Rules: []Rule{{Read: true}, {Prefix: "private", Read: false}}}
	checkout, err := f.e.Checkout(f.ctx, "test", revision, principal, private)
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := f.e.Tree(f.ctx, checkout.Root)
	if _, exists := tree["private"]; exists {
		t.Fatal("private name leaked through filtered view")
	}
	cap, err := VerifyCapability(checkout.Capability, public, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service := NewObjectService("daemon", f.e.Objects, public, "")
	entry, _ := f.e.Lookup(f.ctx, checkout.Root, "public/a")
	if err = service.authorize(f.ctx, cap, ObjectRequest{Kind: "blob", ID: entry.ID, Path: "public/a"}); err != nil {
		t.Fatal(err)
	}
	secret := f.file(t, "secret")
	requireError(t, service.authorize(f.ctx, cap, ObjectRequest{Kind: "blob", ID: secret.ID, Path: "public/a"}), 403)
	requireError(t, service.authorize(f.ctx, cap, ObjectRequest{Kind: "blob", ID: secret.ID, Path: "private/secret"}), 403)
	_, err = VerifyCapability(checkout.Capability+"x", public, time.Now())
	requireError(t, err, 401)
}
func FuzzPaths(f *testing.F) {
	for _, s := range []string{"src/main.go", "../escape", "a//b", ".bough/state", "x\x00y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidPath(s, false) == nil {
			if strings.HasPrefix(s, "/") || strings.Contains(s, "\\") || strings.Contains(s, "\x00") {
				t.Fatal("unsafe path accepted")
			}
		}
	})
}
