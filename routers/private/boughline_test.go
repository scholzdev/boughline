// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package private

import (
	"forgejo.org/services/context"
	"forgejo.org/services/contexttest"
	"testing"
)

func TestBoughSSHAlwaysDisabled(t *testing.T) {
	for _, handler := range []func(*context.PrivateContext){ServCommand, ServNoCommand} {
		ctx, out := contexttest.MockContext(t, "/serv/command/0/owner/project")
		handler(&context.PrivateContext{Base: ctx.Base})
		if out.Code != 403 {
			t.Fatal("SSH transport was accepted", out.Code)
		}
	}
}
