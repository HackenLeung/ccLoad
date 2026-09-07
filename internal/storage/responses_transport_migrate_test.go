//go:build sonic

package storage

import (
	"context"
	"testing"
)

func TestResponsesTransportLegacyMigration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE channels(id INTEGER PRIMARY KEY,name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO channels(id,name) VALUES(1,'legacy')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureColumn(ctx, db, DialectSQLite, "channels", "responses_transport", "VARCHAR(32) NOT NULL DEFAULT 'http'", "TEXT NOT NULL DEFAULT 'http'"); err != nil {
			t.Fatal(err)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `SELECT responses_transport FROM channels WHERE id=1`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "http" {
		t.Fatalf("legacy mode=%s", mode)
	}
}
