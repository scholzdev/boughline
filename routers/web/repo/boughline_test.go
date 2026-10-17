// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package repo

import (
	"forgejo.org/services/context"
	"forgejo.org/services/contexttest"
	"testing"
)

func TestBoughHTTPGitAlwaysDisabled(t *testing.T) {
	for _, handler := range []func(*context.Context){HTTPGitEnabledHandler, ServiceUploadPack, ServiceReceivePack, GetInfoRefs, GetInfoPacks, GetLooseObject, GetPackFile, GetIdxFile, GetTextFile("HEAD")} {
		ctx, out := contexttest.MockContext(t, "/owner/project.git/info/refs")
		handler(ctx)
		if out.Code != 409 {
			t.Fatal("Git transport was accepted", out.Code)
		}
	}
}
