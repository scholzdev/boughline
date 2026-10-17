package boughline

import (
	"fmt"
	"strings"
	"testing"
)

func TestHashKnowledgeCannotPublishPrivateBlobs(t *testing.T) {
	f := setup(t)
	secret := f.file(t, "private source bytes")
	base := f.commit(t, f.genesis, map[string]*File{"private/secret.txt": secret})
	attacker := Principal{Name: "agent", Rules: []Rule{{Prefix: "public", Read: true, Write: true}}}
	request := CommitRequest{Parents: []string{base}, Changes: map[string]*File{"public/leak.txt": secret}, Message: "hash guess", RequestID: "hash-guess"}
	_, err := f.e.Commit(f.ctx, "test", attacker, request)
	requireError(t, err, 403)
	second, err := f.e.CreateRepo(f.ctx, RepoConfig{ID: "second", Owner: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	request.Parents = []string{second}
	_, err = f.e.Commit(f.ctx, "second", Principal{Name: "agent", Admin: true}, request)
	requireError(t, err, 403)
	public := []byte("provided source bytes")
	id, err := f.e.UploadBlob(f.ctx, "test", attacker, public)
	if err != nil {
		t.Fatal(err)
	}
	request.Parents = []string{base}
	request.Changes = map[string]*File{"public/file.txt": {ID: id, Mode: Regular, Size: int64(len(public))}}
	request.RequestID = "actual-bytes"
	if _, err = f.e.Commit(f.ctx, "test", attacker, request); err != nil {
		t.Fatal(err)
	}
}
func TestRestrictedDiffDoesNotRevealPrivateNames(t *testing.T) {
	f := setup(t)
	revision := f.commit(t, f.genesis, map[string]*File{"secret-unlisted/hidden-name": f.file(t, "hidden")})
	p := Principal{Name: "reader", Rules: []Rule{{Prefix: "public", Read: true}}}
	_, err := f.e.Diff(f.ctx, "test", f.genesis, revision, p)
	requireError(t, err, 403)
	if strings.Contains(fmt.Sprint(err), "hidden-name") || strings.Contains(string(Encode(err)), "secret-unlisted") {
		t.Fatal("comparison leaked a denied path", err)
	}
}

func TestInvalidSymlinksFailBeforePublication(t *testing.T) {
	f := setup(t)
	for _, data := range []string{"", "bad\x00target", strings.Repeat("x", 4097)} {
		file := f.file(t, data)
		file.Mode = Symlink
		_, err := f.e.Commit(f.ctx, "test", f.p, CommitRequest{Parents: []string{f.genesis}, Changes: map[string]*File{"link": file}, Message: "invalid link", RequestID: RequestID()})
		requireError(t, err, 400)
	}
}

func TestStackedReviewsRespectScopeAuthorshipAndRevocation(t *testing.T) {
	f := setup(t)
	if err := f.e.Meta.SetPolicies(f.ctx, "test", []ReviewPolicy{{Prefix: "security", Approvals: 1}}); err != nil {
		t.Fatal(err)
	}
	protected := f.commit(t, f.genesis, map[string]*File{"security/f": f.file(t, "sensitive")})
	other := Principal{Name: "other", Admin: true}
	blob, err := f.e.UploadBlob(f.ctx, "test", other, []byte("public"))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := f.e.Commit(f.ctx, "test", other, CommitRequest{Parents: []string{protected}, Changes: map[string]*File{"public/f": {ID: blob, Mode: Regular, Size: 6}}, Message: "later independent change", RequestID: "stack"})
	if err != nil {
		t.Fatal(err)
	}
	partial := Principal{Name: "reviewer", Rules: []Rule{{Prefix: "public", Read: true}}}
	if err = f.e.Approve(f.ctx, "test", stack, true, partial); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Land(f.ctx, "test", "main", f.genesis, stack, "denied-scope", f.p)
	requireError(t, err, 403)
	// The original protected author cannot approve their own edit by stacking a
	// later commit authored by someone else above it.
	if err = f.e.Approve(f.ctx, "test", stack, true, f.p); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Land(f.ctx, "test", "main", f.genesis, stack, "denied-self", f.p)
	requireError(t, err, 403)
	full := Principal{Name: "reviewer", Rules: []Rule{{Read: true}}}
	if err = f.e.Approve(f.ctx, "test", stack, true, full); err != nil {
		t.Fatal(err)
	}
	guard, err := f.e.checkReview(f.ctx, "test", f.genesis, stack, stack, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.Meta.SetGrant(f.ctx, "test", partial); err != nil {
		t.Fatal(err)
	}
	requireError(t, f.e.Meta.CAS(f.ctx, "test", "main", f.genesis, stack, guard), 409)
	_, err = f.e.Land(f.ctx, "test", "main", f.genesis, stack, "denied-revoked", f.p)
	requireError(t, err, 403)
	if err = f.e.Meta.SetGrant(f.ctx, "test", full); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Land(f.ctx, "test", "main", f.genesis, stack, "approved", f.p); err != nil {
		t.Fatal(err)
	}
}
