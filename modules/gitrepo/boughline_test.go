// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package gitrepo_test

import (
	"context"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/gitrepo"
	"strings"
	"testing"
)

func TestBoughGitTransportGuard(t *testing.T) {
	for _, native := range []bool{false, true} {
		repo := &repo_model.Repository{IsNative: native, Name: "project", OwnerName: "owner"}
		_, err := gitrepo.OpenRepository(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "native Boughline") {
			t.Fatal("Git storage remained available", native, err)
		}
		if _, err = gitrepo.OpenWikiRepository(context.Background(), repo); err == nil {
			t.Fatal("Git wiki storage remained available", native)
		}
	}
}
