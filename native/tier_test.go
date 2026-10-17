package boughline

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTierRolesCannotBypassHierarchy(t *testing.T) {
	f := setup(t)
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	id, err := PutRaw(f.ctx, f.e.Objects, "blob", []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 40)
	for _, role := range []string{"server", "cache", "daemon"} {
		service := NewObjectService(role, f.e.Objects, public, secret)
		for _, caller := range []string{"", "cache", "daemon"} {
			request := httptest.NewRequest("POST", "http://localhost/v1/objects/raw", bytes.NewReader(Encode(ObjectRequest{Kind: "blob", ID: id})))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Bough-Tier-Key", secret)
			request.Header.Set("X-Bough-Caller", caller)
			out := httptest.NewRecorder()
			service.Handler(out, request)
			expected := http.StatusForbidden
			if role == "server" && caller == "cache" || role == "cache" && caller == "daemon" {
				expected = http.StatusOK
			}
			if out.Code != expected {
				t.Fatalf("role %s caller %s: wanted %d got %d", role, caller, expected, out.Code)
			}
		}
	}
}
