// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: MIT

package repository

import (
	"context"
	"fmt"
	"strings"

	"forgejo.org/models/db"
	repo_model "forgejo.org/models/repo"
	user_model "forgejo.org/models/user"
	repo_module "forgejo.org/modules/repository"
	api "forgejo.org/modules/structs"
)

// CreateRepoOptions contains the create repository options
type CreateRepoOptions struct {
	Name             string
	Description      string
	OriginalURL      string
	GitServiceType   api.GitServiceType
	Gitignores       string
	IssueLabels      string
	License          string
	Readme           string
	DefaultBranch    string
	IsPrivate        bool
	IsMirror         bool
	IsTemplate       bool
	AutoInit         bool
	Status           repo_model.RepositoryStatus
	TrustModel       repo_model.TrustModelType
	MirrorInterval   string
	ObjectFormatName string
	Website          string
}

// CreateRepositoryDirectly creates only native project metadata. No Git directory,
// refs, hooks, initial commit or object database is created by this forge.
func CreateRepositoryDirectly(ctx context.Context, doer, owner *user_model.User, opts CreateRepoOptions) (*repo_model.Repository, error) {
	if opts.IsMirror || opts.IsTemplate || opts.AutoInit || opts.OriginalURL != "" || opts.Gitignores != "" || opts.License != "" || opts.Readme != "" || opts.ObjectFormatName != "" || opts.DefaultBranch != "" && opts.DefaultBranch != "main" {
		return nil, fmt.Errorf("Boughline creates empty native projects on main; Git initialization, mirrors and templates are unsupported")
	}
	if !doer.IsAdmin && !owner.CanCreateRepo() {
		return nil, repo_model.ErrReachLimitOfRepo{Limit: owner.MaxRepoCreation}
	}
	repo := &repo_model.Repository{
		OwnerID: owner.ID, Owner: owner, OwnerName: owner.Name,
		Name: opts.Name, LowerName: strings.ToLower(opts.Name), Description: opts.Description,
		IsNative: true, IsPrivate: opts.IsPrivate, IsEmpty: true,
		DefaultBranch: "main", TrustModel: repo_model.DefaultTrustModel, Website: opts.Website,
	}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		if err := repo_module.CreateRepositoryByExample(ctx, doer, owner, repo, false, false); err != nil {
			return err
		}
		if opts.IssueLabels != "" {
			return repo_module.InitializeLabels(ctx, repo.ID, opts.IssueLabels, false)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return repo, nil
}
