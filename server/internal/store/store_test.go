package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	for range 2 {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
			t.Fatalf("user_version = %d, %v", v, err)
		}
		var fk int
		db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
		if fk != 1 {
			t.Fatal("foreign_keys off")
		}
		var auto string
		if err := db.QueryRow("SELECT value FROM settings WHERE key='auto_approve'").Scan(&auto); err != nil || auto != "false" {
			t.Fatalf("settings seed: %q %v", auto, err)
		}
		db.Close()
	}
}
