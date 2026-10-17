// Copyright 2026 The Boughline Authors.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import "code.forgejo.org/xorm/xorm"

func init() {
	registerMigration(&Migration{Description: "add native Boughline repository storage", Upgrade: addNativeRepository})
}

func addNativeRepository(x *xorm.Engine) error {
	type Repository struct {
		IsNative bool `xorm:"NOT NULL DEFAULT false"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, &Repository{})
	return err
}
