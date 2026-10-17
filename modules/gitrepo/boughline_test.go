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
	_, err := gitrepo.OpenRepository(context.Background(), &repo_model.Repository{IsNative: true, Name: "native", OwnerName: "owner"})
	if err == nil || !strings.Contains(err.Error(), "native Boughline") {
		t.Fatal("Git opened native source", err)
	}
}
