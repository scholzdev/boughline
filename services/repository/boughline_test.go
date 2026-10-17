// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package repository

import (
	"context"
	"testing"
)

func TestBoughGitCreationOptionsRejectedBeforeStorage(t *testing.T) {
	for _, options := range []CreateRepoOptions{
		{AutoInit: true}, {IsMirror: true}, {IsTemplate: true}, {Gitignores: "Go"},
		{Readme: "Default"}, {License: "MIT"}, {ObjectFormatName: "sha1"},
		{OriginalURL: "https://example.invalid/repo.git"}, {DefaultBranch: "legacy"},
	} {
		if _, err := CreateRepositoryDirectly(context.Background(), nil, nil, options); err == nil {
			t.Fatal("Git options reached storage", options)
		}
	}
}
