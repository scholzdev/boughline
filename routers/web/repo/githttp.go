// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: MIT

package repo

import (
	"forgejo.org/services/context"
	"net/http"
)

func HTTPGitEnabledHandler(ctx *context.Context) {
	ctx.PlainText(http.StatusConflict, "Boughline uses the bough client; Git HTTP transport is disabled")
}
func CorsHandler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}
func ServiceUploadPack(ctx *context.Context)      { HTTPGitEnabledHandler(ctx) }
func ServiceReceivePack(ctx *context.Context)     { HTTPGitEnabledHandler(ctx) }
func GetInfoRefs(ctx *context.Context)            { HTTPGitEnabledHandler(ctx) }
func GetTextFile(_ string) func(*context.Context) { return HTTPGitEnabledHandler }
func GetInfoPacks(ctx *context.Context)           { HTTPGitEnabledHandler(ctx) }
func GetLooseObject(ctx *context.Context)         { HTTPGitEnabledHandler(ctx) }
func GetPackFile(ctx *context.Context)            { HTTPGitEnabledHandler(ctx) }
func GetIdxFile(ctx *context.Context)             { HTTPGitEnabledHandler(ctx) }
