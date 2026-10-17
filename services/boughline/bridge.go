// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later

package boughline

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/setting"
	native "github.com/scholzdev/boughline/native"
)

var transportOnce sync.Once
var backendHTTP *http.Client
var backendErr error

func Configured() error {
	if len(setting.Repository.BoughlineSecret) < 32 {
		return fmt.Errorf("repository.BOUGHLINE_SECRET must match BOUGH_FORGEJO_SECRET and contain at least 32 characters")
	}
	return native.ValidateOrigin(setting.Repository.BoughlineURL)
}
func ID(repo *repo_model.Repository) string { return fmt.Sprintf("repo-%d", repo.ID) }
func Session(repo *repo_model.Repository, subject string, write, admin bool) string {
	return native.SignForgeSession(native.ForgeSession{Repo: ID(repo), Subject: subject, Write: write && !repo.IsArchived, Admin: admin && !repo.IsArchived, Expires: time.Now().Add(30 * time.Second).Unix()}, setting.Repository.BoughlineSecret)
}
func HTTP() (*http.Client, error) {
	transportOnce.Do(func() { backendHTTP, backendErr = native.SecureHTTP(setting.Repository.BoughlineCAFile) })
	return backendHTTP, backendErr
}
func Client(repo *repo_model.Repository, subject string, write, admin bool) (*native.Client, error) {
	if err := Configured(); err != nil {
		return nil, err
	}
	h, err := HTTP()
	if err != nil {
		return nil, err
	}
	return &native.Client{Server: setting.Repository.BoughlineURL, Repo: ID(repo), Token: Session(repo, subject, write, admin), HTTP: h}, nil
}
func Initialize(ctx context.Context, repo *repo_model.Repository) error {
	c, err := Client(repo, fmt.Sprintf("forge-user-%d", repo.OwnerID), true, true)
	if err != nil {
		return err
	}
	return c.Request(ctx, "POST", "/v1/repos/"+ID(repo)+"/initialize", native.RepoConfig{ID: ID(repo), Owner: fmt.Sprintf("forge-user-%d", repo.OwnerID), ForgejoOnly: true}, nil)
}
