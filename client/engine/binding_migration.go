package engine

import (
	"context"
	"database/sql"
	"errors"
)

// Keep historical snapshots keyed to the original unit. An archived binding has
// a NULL active path, so SQLite's unique index still enforces one owner per path.
const bindingColumns = "(unit TEXT PRIMARY KEY,path TEXT UNIQUE,profile TEXT NOT NULL,base TEXT,generation TEXT NOT NULL DEFAULT '')"

func migrateBindings(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var current bool
	check := func() error {
		return conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('bindings') WHERE name='generation')").Scan(&current)
	}
	if err = check(); err != nil || current {
		return err
	}
	// SQLite table rebuild procedure: keep the referenced table's name stable,
	// then verify all foreign keys before exposing the change to other processes.
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	if err = check(); err != nil {
		return err
	}
	if !current {
		if _, err = conn.ExecContext(ctx, `CREATE TABLE migrated_bindings `+bindingColumns+`;
   INSERT INTO migrated_bindings(unit,path,profile,base) SELECT unit,path,profile,base FROM bindings;
   DROP TABLE bindings; ALTER TABLE migrated_bindings RENAME TO bindings;`); err != nil {
			return err
		}
		var invalid bool
		if err = conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check)").Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return errors.New("绑定迁移的外键校验失败")
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
