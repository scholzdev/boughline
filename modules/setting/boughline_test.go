// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later
package setting

import (
	"slices"
	"testing"
)

func TestBoughCannotReenableGitHosting(t *testing.T) {
	oldRepo, oldSSH, oldMirror := Repository, SSH, Mirror
	t.Cleanup(func() { Repository, SSH, Mirror = oldRepo, oldSSH, oldMirror })
	cfg, err := NewConfigProviderFromData(`
[repository]
DISABLE_HTTP_GIT=false
DISABLE_MIGRATIONS=false
DISABLE_FORKS=false
ENABLE_PUSH_CREATE_USER=true
ENABLE_PUSH_CREATE_ORG=true
[server]
DISABLE_SSH=false
START_SSH_SERVER=true
[mirror]
ENABLED=true
`)
	if err != nil {
		t.Fatal(err)
	}
	loadRepositoryFrom(cfg)
	loadSSHFrom(cfg)
	loadMirrorFrom(cfg)
	if !Repository.DisableHTTPGit || !Repository.DisableMigrations || !Repository.DisableForks || Repository.EnablePushCreateUser || Repository.EnablePushCreateOrg || !SSH.Disabled || SSH.StartBuiltinServer || Mirror.Enabled {
		t.Fatal("configuration restored Git hosting")
	}
	for _, unit := range []string{"repo.pulls", "repo.wiki", "repo.releases", "repo.actions"} {
		if !slices.Contains(Repository.DisabledRepoUnits, unit) {
			t.Fatal("Git unit remains enabled", unit)
		}
	}
}
