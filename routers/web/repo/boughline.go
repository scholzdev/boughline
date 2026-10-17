// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later

package repo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"forgejo.org/models/unit"
	"forgejo.org/modules/base"
	bough_service "forgejo.org/services/boughline"
	"forgejo.org/services/context"
	native "github.com/scholzdev/boughline/native"
)

type boughEntry struct {
	Name, Kind, Href string
	Size             int64
}
type boughPatch struct{ Path, Text string }
type boughLine struct {
	Number, Original int
	Commit, Text     string
}

func boughClient(ctx *context.Context) (*native.Client, error) {
	subject := "forge-anonymous"
	if ctx.Doer != nil {
		subject = fmt.Sprintf("forge-user-%d", ctx.Doer.ID)
	}
	return bough_service.Client(ctx.Repo.Repository, subject, ctx.Repo.CanWrite(unit.TypeCode), ctx.Repo.IsAdmin())
}
func boughLink(link, revision, filename, view string) string {
	return link + "?" + url.Values{"revision": {revision}, "path": {filename}, "view": {view}}.Encode()
}
func BoughHome(ctx *context.Context) {
	if !ctx.Repo.CanRead(unit.TypeCode) {
		ctx.NotFound("Code", nil)
		return
	}
	c, err := boughClient(ctx)
	if err != nil {
		ctx.ServerError("Boughline", err)
		return
	}
	revision := ctx.FormString("revision")
	if revision == "" {
		revision = "main"
	}
	revision, err = c.Resolve(ctx, revision)
	if err != nil {
		ctx.ServerError("Boughline revision", err)
		return
	}
	filename, view := ctx.FormString("path"), ctx.FormString("view")
	if view == "" {
		view = "browse"
	}
	route := "/v1/repos/" + c.Repo + "/"
	link := ctx.Repo.RepoLink
	ctx.Data["BoughRevision"], ctx.Data["BoughPath"], ctx.Data["BoughView"] = revision, filename, view
	ctx.Data["BoughBrowseLink"] = boughLink(link, revision, filename, "browse")
	ctx.Data["BoughHistoryLink"] = boughLink(link, revision, filename, "history")
	ctx.Data["BoughBlameLink"] = boughLink(link, revision, filename, "blame")
	ctx.Data["BoughDiffLink"] = boughLink(link, revision, "", "diff")
	ctx.Data["CanWriteCode"] = ctx.Repo.CanWrite(unit.TypeCode) && !ctx.Repo.Repository.IsArchived
	switch view {
	case "browse":
		var result native.BrowseResult
		query := url.Values{"revision": {revision}, "path": {filename}, "cursor": {ctx.FormString("cursor")}}
		err = c.Request(ctx, "GET", route+"browse?"+query.Encode(), nil, &result)
		if err != nil {
			break
		}
		entries := []boughEntry{}
		for _, entry := range result.Entries {
			entries = append(entries, boughEntry{Name: entry.Name, Kind: entry.Entry.Kind, Size: entry.Entry.Size, Href: boughLink(link, revision, path.Join(filename, entry.Name), "browse")})
		}
		ctx.Data["BoughEntries"] = entries
		if filename != "" {
			parent := path.Dir(filename)
			if parent == "." {
				parent = ""
			}
			ctx.Data["BoughParent"] = boughLink(link, revision, parent, "browse")
		}
		if result.Next != "" {
			query.Set("cursor", result.Next)
			query.Set("view", "browse")
			ctx.Data["BoughNext"] = link + "?" + query.Encode()
		}
		if result.File != nil {
			ctx.Data["BoughFile"] = result.File
			ctx.Data["BoughTruncated"] = result.Truncated
			ctx.Data["BoughBinary"] = strings.ContainsRune(string(result.Content), 0)
			ctx.Data["BoughText"] = string(result.Content)
		}
	case "diff":
		from := ctx.FormString("base")
		if from == "" {
			from = "main"
		}
		ctx.Data["BoughBase"] = from
		var baseID string
		baseID, err = c.Resolve(ctx, from)
		if err != nil {
			break
		}
		var result struct {
			Changes map[string]native.Difference `json:"changes"`
			Patches map[string]string            `json:"patches"`
		}
		err = c.Request(ctx, "POST", route+"diff?patch=true", map[string]string{"base": baseID, "target": revision}, &result)
		patches := []boughPatch{}
		for filename, patch := range result.Patches {
			patches = append(patches, boughPatch{filename, patch})
		}
		sort.Slice(patches, func(i, j int) bool { return patches[i].Path < patches[j].Path })
		ctx.Data["BoughPatches"] = patches
	case "history":
		var result struct {
			History []native.HistoryNode `json:"history"`
			Next    string               `json:"next"`
		}
		query := url.Values{"revision": {revision}, "path": {filename}, "cursor": {ctx.FormString("cursor")}}
		err = c.Request(ctx, "GET", route+"history?"+query.Encode(), nil, &result)
		ctx.Data["BoughHistory"] = result.History
		if result.Next != "" {
			query.Set("cursor", result.Next)
			query.Set("view", "history")
			ctx.Data["BoughNext"] = link + "?" + query.Encode()
		}
	case "blame":
		var blame native.Blame
		var content native.BrowseResult
		query := url.Values{"revision": {revision}, "path": {filename}}
		err = c.Request(ctx, "GET", route+"blame?"+query.Encode(), nil, &blame)
		if err != nil {
			break
		}
		err = c.Request(ctx, "GET", route+"browse?"+query.Encode(), nil, &content)
		if err != nil {
			break
		}
		if content.Truncated {
			err = fmt.Errorf("file exceeds web blame display limit")
			break
		}
		lines := []boughLine{}
		text := strings.SplitAfter(string(content.Content), "\n")
		if len(text) > 0 && text[len(text)-1] == "" {
			text = text[:len(text)-1]
		}
		for i, line := range text {
			if i >= len(blame.Origins) {
				err = fmt.Errorf("blame length mismatch")
				break
			}
			origin := blame.Origins[i]
			lines = append(lines, boughLine{Number: i + 1, Original: origin.Line, Commit: origin.Commit, Text: line})
		}
		ctx.Data["BoughLines"] = lines
	case "search":
		q := ctx.FormString("q")
		ctx.Data["BoughQuery"] = q
		var result struct {
			Paths []string `json:"paths"`
		}
		err = c.Request(ctx, "GET", route+"search?"+url.Values{"revision": {revision}, "q": {q}}.Encode(), nil, &result)
		entries := []boughEntry{}
		for _, filename := range result.Paths {
			entries = append(entries, boughEntry{Name: filename, Kind: "file", Href: boughLink(link, revision, filename, "browse")})
		}
		ctx.Data["BoughEntries"] = entries
	default:
		err = fmt.Errorf("unknown native view")
	}
	if err != nil {
		ctx.Data["BoughError"] = err.Error()
	}
	var changes any
	if err = c.Request(ctx, "GET", route+"changes/"+revision, nil, &changes); err == nil {
		data, _ := json.MarshalIndent(changes, "", "  ")
		ctx.Data["BoughChange"] = string(data)
	}
	ctx.HTML(http.StatusOK, base.TplName("repo/boughline"))
}

func BoughAction(ctx *context.Context) {
	if !ctx.Repo.Repository.IsNative || ctx.Doer == nil || !ctx.Repo.CanRead(unit.TypeCode) || ctx.Repo.Repository.IsArchived {
		ctx.PlainText(http.StatusForbidden, "Native project access required")
		return
	}
	c, err := boughClient(ctx)
	if err != nil {
		ctx.ServerError("Boughline", err)
		return
	}
	revision, err := c.Resolve(ctx, ctx.FormString("revision"))
	if err != nil {
		ctx.ServerError("Boughline revision", err)
		return
	}
	switch ctx.Params(":operation") {
	case "review":
		err = c.Request(ctx, "POST", "/v1/repos/"+c.Repo+"/review", map[string]any{"revision": revision, "approve": ctx.FormString("approve") == "true"}, nil)
	case "land":
		if !ctx.Repo.CanWrite(unit.TypeCode) {
			ctx.PlainText(http.StatusForbidden, "Native write permission required")
			return
		}
		revision, err = c.Land(ctx, revision, "main")
	default:
		ctx.NotFound("Native operation", nil)
		return
	}
	if err != nil {
		ctx.PlainText(http.StatusConflict, err.Error())
		return
	}
	ctx.Redirect(boughLink(ctx.Repo.RepoLink, revision, "", "browse"))
}
