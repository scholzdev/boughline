// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package repo

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgejo.org/models/perm"
	access_model "forgejo.org/models/perm/access"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unit"
	user_model "forgejo.org/models/user"
	"forgejo.org/modules/setting"
	bough_service "forgejo.org/services/boughline"
	app_context "forgejo.org/services/context"
	"forgejo.org/services/contexttest"
	native "github.com/scholzdev/boughline/native"
)

func TestBoughGatewayPermissionsAndCredentials(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	engine := native.NewEngine(native.NewMemoryMetadata(), &native.DiskObjects{Directory: t.TempDir()})
	api, err := native.NewAPI(engine, key, strings.Repeat("a", 40), "operator", strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("f", 40)
	api.ForgeSecret = secret
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer forge:") {
			t.Error("Forgejo credentials crossed backend boundary")
		}
		api.ServeHTTP(out, r)
	}))
	defer server.Close()
	oldURL, oldSecret := setting.Repository.BoughlineURL, setting.Repository.BoughlineSecret
	setting.Repository.BoughlineURL, setting.Repository.BoughlineSecret = server.URL, secret
	t.Cleanup(func() { setting.Repository.BoughlineURL, setting.Repository.BoughlineSecret = oldURL, oldSecret })
	repo := &repo_model.Repository{ID: 42, OwnerID: 1, OwnerName: "owner", Name: "native", IsNative: true}
	if err = bough_service.Initialize(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	invoke := func(method, resource string, mode perm.AccessMode, nativeID string) *httptest.ResponseRecorder {
		ctx, out := contexttest.MockAPIContext(t, method+" /api/v1/repos/owner/native/bough/v1/repos/"+nativeID+"/"+resource)
		ctx.SetDoer(&user_model.User{ID: 1, Name: "owner"})
		ctx.SetRepo(&app_context.Repository{Repository: repo, Permission: access_model.Permission{AccessMode: mode, UnitsMode: map[unit.Type]perm.AccessMode{unit.TypeCode: mode}}})
		ctx.SetParams("nativeid", nativeID)
		ctx.SetParams("*", resource)
		ctx.Req.Body = io.NopCloser(bytes.NewBufferString("contents"))
		ctx.Req.ContentLength = 8
		ctx.Req.Header.Set("Authorization", "token forgejo-PAT")
		ctx.Req.Header.Set("Cookie", "session=private")
		BoughProxy(ctx)
		return out
	}
	if out := invoke("GET", "info", perm.AccessModeRead, "repo-42"); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out := invoke("POST", "blobs", perm.AccessModeRead, "repo-42"); out.Code != 403 {
		t.Fatal("reader wrote native source", out.Code)
	}
	if out := invoke("POST", "blobs", perm.AccessModeOwner, "repo-42"); out.Code != 201 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out := invoke("GET", "info", perm.AccessModeOwner, "repo-99"); out.Code != 404 {
		t.Fatal("cross repository proxy", out.Code)
	}
	repo.IsArchived = true
	if out := invoke("POST", "blobs", perm.AccessModeOwner, "repo-42"); out.Code != 403 {
		t.Fatal("archived source was writable", out.Code)
	}
}
