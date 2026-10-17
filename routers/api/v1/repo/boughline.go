// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later

package repo

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"forgejo.org/models/unit"
	"forgejo.org/modules/setting"
	bough_service "forgejo.org/services/boughline"
	"forgejo.org/services/context"
	native "github.com/scholzdev/boughline/native"
)

func BoughInitialize(ctx *context.APIContext) {
	repo := ctx.Repo().Repository
	if !repo.IsNative || repo.IsArchived {
		ctx.Error(http.StatusConflict, "", "only a new native project can be initialized")
		return
	}
	if err := bough_service.Initialize(ctx, repo); err != nil {
		ctx.Error(http.StatusServiceUnavailable, "Boughline", err)
		return
	}
	BoughInfo(ctx)
}
func BoughInfo(ctx *context.APIContext) {
	repo := ctx.Repo().Repository
	if !repo.IsNative {
		ctx.Error(http.StatusConflict, "", "this is a Git repository")
		return
	}
	ctx.JSON(http.StatusOK, map[string]string{"repo": bough_service.ID(repo), "server": strings.TrimRight(setting.AppURL, "/") + "/api/v1/repos/" + url.PathEscape(repo.OwnerName) + "/" + url.PathEscape(repo.Name) + "/bough", "transport": "boughline-v1"})
}
func BoughProxy(ctx *context.APIContext) {
	repo := ctx.Repo().Repository
	if !repo.IsNative || ctx.Params(":nativeid") != bough_service.ID(repo) {
		ctx.Error(http.StatusNotFound, "", "native repository not found")
		return
	}
	resource := ctx.Params("*")
	if resource == "" || strings.Contains(resource, "..") || strings.Contains(resource, "\\") {
		ctx.Error(http.StatusBadRequest, "", "invalid native route")
		return
	}
	// The gateway authenticates every request against current repository permissions.
	// No Forgejo password, PAT, or cookie reaches the native backend.
	readOperation := ctx.Req.Method == "GET" || ctx.Req.Method == "POST" && (resource == "checkout" || resource == "diff" || resource == "merge-base")
	if !readOperation && (ctx.Doer() == nil || !ctx.Permission().CanWrite(unit.TypeCode) || repo.IsArchived) {
		ctx.Error(http.StatusForbidden, "", "native write permission required")
		return
	}
	if err := bough_service.Configured(); err != nil {
		ctx.Error(http.StatusServiceUnavailable, "Boughline", err)
		return
	}
	subject := "forge-anonymous"
	if ctx.Doer() != nil {
		subject = fmt.Sprintf("forge-user-%d", ctx.Doer().ID)
	}
	origin, _ := url.Parse(setting.Repository.BoughlineURL)
	target := *ctx.Req.URL
	target.Scheme = origin.Scheme
	target.Host = origin.Host
	target.Path = "/v1/repos/" + bough_service.ID(repo) + "/" + resource
	target.RawPath = ""
	request := ctx.Req.Clone(ctx)
	request.URL = &target
	request.RequestURI = ""
	request.Host = origin.Host
	request.Header = request.Header.Clone()
	request.Header.Del("Cookie")
	request.Header.Set("Authorization", "Bearer "+bough_service.Session(repo, subject, ctx.Permission().CanWrite(unit.TypeCode), ctx.Permission().IsAdmin()))
	limit := int64(native.MaxRequest)
	if resource == "blobs" {
		limit = native.MaxBlob
	}
	request.Body = http.MaxBytesReader(ctx.Resp, request.Body, limit)
	h, err := bough_service.HTTP()
	if err != nil {
		ctx.Error(http.StatusServiceUnavailable, "Boughline", err)
		return
	}
	response, err := h.Do(request)
	if err != nil {
		ctx.Error(http.StatusServiceUnavailable, "", "native backend unavailable")
		return
	}
	defer response.Body.Close()
	ctx.Resp.Header().Set("Content-Type", "application/json")
	ctx.Resp.Header().Set("Cache-Control", "private, no-store")
	ctx.Resp.Header().Set("X-Content-Type-Options", "nosniff")
	ctx.Resp.WriteHeader(response.StatusCode)
	_, _ = io.Copy(ctx.Resp, io.LimitReader(response.Body, native.MaxRequest+1))
}
