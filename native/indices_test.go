package boughline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathIndicesSurviveDeletesAndHideDeniedCopies(t *testing.T) {
	f := setup(t)
	one := f.file(t, "same")
	a := f.commit(t, f.genesis, map[string]*File{"source/main.go": one, "public/copy.go": one, "private/copy.go": one, "old/file.go": f.file(t, "other")})
	b := f.commit(t, a, map[string]*File{"source/main.go": f.file(t, "changed"), "public/copy.go": nil, "new/copy.go": one, "old/file.go": nil})
	reader := Principal{Name: "reader", Rules: []Rule{{Read: true}, {Prefix: "private", Read: false}}}
	paths, err := f.e.SearchPaths(f.ctx, "test", b, "COPY.go", 100, reader)
	if err != nil || strings.Join(paths, ",") != "new/copy.go" {
		t.Fatal(paths, err)
	}
	paths, err = f.e.Copies(f.ctx, "test", a, "source/main.go", 100, reader)
	if err != nil || strings.Join(paths, ",") != "public/copy.go" {
		t.Fatal(paths, err)
	}
	paths, err = f.e.Copies(f.ctx, "test", b, "new/copy.go", 100, reader)
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
	paths, err = f.e.SearchPaths(f.ctx, "test", b, "old", 100, reader)
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}
func TestImportGitPreservesDAGModesMetadataAndResumes(t *testing.T) {
	f := setup(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	api, err := NewAPI(f.e, key, strings.Repeat("a", 40), "owner", strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Server: "http://localhost", Repo: "test", Token: strings.Repeat("a", 40), HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := httptest.NewRecorder()
		api.ServeHTTP(out, r)
		return out.Result(), nil
	})}}
	source := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", source}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Importer Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Importer Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-b", "main")
	git("config", "commit.gpgsign", "false")
	git("config", "core.hooksPath", filepath.Join(source, "no-hooks"))
	if err = os.WriteFile(filepath.Join(source, "file"), []byte("initial\n"), 0755); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "first")
	first := git("rev-parse", "HEAD")
	git("checkout", "-b", "side")
	if err = os.Symlink("file", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "symlink")
	side := git("rev-parse", "HEAD")
	git("checkout", "main")
	if err = os.WriteFile(filepath.Join(source, "another"), []byte("other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "parallel")
	parallel := git("rev-parse", "HEAD")
	git("merge", "--no-ff", "side", "-m", "merge both")
	head := git("rev-parse", "HEAD")
	git("tag", "-a", "v1", "-m", "release")
	checkpoint := filepath.Join(t.TempDir(), "import.json")
	refs, err := ImportGit(context.Background(), c, source, checkpoint, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/main"] != "main" || refs["refs/heads/side"] == "" || refs["refs/tags/v1"] == "" {
		t.Fatal(refs)
	}
	main, err := c.Resolve(f.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	change, err := f.e.Change(f.ctx, "test", main)
	if err != nil {
		t.Fatal(err)
	}
	if change.OriginalGit.ID != head || len(change.Parents) != 2 || len(change.OriginalGit.Raw) == 0 {
		t.Fatal("lost merge metadata", change)
	}
	for i, want := range []string{parallel, side} {
		parent, err := f.e.Change(f.ctx, "test", change.Parents[i])
		if err != nil || parent.OriginalGit.ID != want {
			t.Fatal("parent order lost", i, err)
		}
	}
	root, _ := f.e.Root(f.ctx, "test", main)
	entry, _ := f.e.Lookup(f.ctx, root, "file")
	if entry.Mode != Executable {
		t.Fatal("executable mode lost")
	}
	entry, _ = f.e.Lookup(f.ctx, root, "link")
	if entry.Mode != Symlink {
		t.Fatal("symlink mode lost")
	}
	if _, err = ImportGit(f.ctx, c, source, checkpoint, "refs/heads/main"); err != nil {
		t.Fatal("resume", err)
	}
	if git("rev-parse", "HEAD") != head || git("status", "--porcelain") != "" {
		t.Fatal("source repository modified")
	}
	var state importCheckpoint
	raw, _ := os.ReadFile(checkpoint)
	_ = jsonDecode(raw, &state)
	if state.Commits[first] == "" {
		t.Fatal("root history lost")
	}
}
func TestLegalLongPathsRetainHistory(t *testing.T) {
	f := setup(t)
	path := strings.Repeat("x", 255)
	revision := f.commit(t, f.genesis, map[string]*File{path: f.file(t, "long")})
	nodes, _, err := f.e.History(f.ctx, "test", revision, path, "", 10, f.p)
	if err != nil || len(nodes) != 1 {
		t.Fatal(nodes, err)
	}
}
