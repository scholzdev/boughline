// Copyright 2019 The Gitea Authors. All rights reserved.
// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: MIT

package private

import (
	"forgejo.org/modules/private"
	"forgejo.org/services/context"
	"net/http"
)

// Legacy SSH transport is disabled for the entire native-only forge, including
// push-to-create and wiki repositories, before looking up any source storage.
func ServNoCommand(ctx *context.PrivateContext) {
	ctx.JSON(http.StatusForbidden, private.Response{UserMsg: "Boughline uses the bough client; Git SSH transport is disabled"})
}
func ServCommand(ctx *context.PrivateContext) { ServNoCommand(ctx) }
