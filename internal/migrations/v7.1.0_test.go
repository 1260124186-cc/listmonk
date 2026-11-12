package migrations

import (
	"log"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
)

const migrationTestDSN = "postgres://listmonk:listmonk@127.0.0.1:15545/listmonk_test?sslmode=disable"

// TestV7_1_0 verifies the template bundle columns are added to a pre-v7.1.0
// templates table and that the migration is idempotent.
func TestV7_1_0(t *testing.T) {
	dsn := os.Getenv("LISTMONK_TEST_DSN")
	if dsn == "" {
		dsn = migrationTestDSN
	}

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Skipf("test database not available (%v); set LISTMONK_TEST_DSN to run", err)
	}
	defer db.Close()

	// Recreate the legacy (v7.0.0) templates table without the new columns.
	if _, err := db.Exec(`
		DROP TABLE IF EXISTS templates CASCADE;
		CREATE TABLE templates (
			id              SERIAL PRIMARY KEY,
			name            TEXT NOT NULL,
			type            TEXT NOT NULL DEFAULT 'campaign',
			subject         TEXT NOT NULL,
			body            TEXT NOT NULL,
			body_source     TEXT NULL,
			is_default      BOOLEAN NOT NULL DEFAULT false,
			created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			updated_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);
		CREATE UNIQUE INDEX ON templates (is_default) WHERE is_default = true;
		INSERT INTO templates (name, subject, body) VALUES ('legacy', '', 'body');
	`); err != nil {
		t.Fatalf("recreating legacy table: %v", err)
	}

	// First run.
	if err := V7_1_0(db, nil, nil, log.New(os.Stdout, "", log.LstdFlags)); err != nil {
		t.Fatalf("first migration run: %v", err)
	}

	// Columns and partial unique index exist; existing rows default to
	// unmanaged with a NULL key.
	var managed bool
	var keyValid bool
	if err := db.Get(&managed, `SELECT managed FROM templates WHERE name='legacy'`); err != nil {
		t.Fatalf("managed column missing: %v", err)
	}
	if managed {
		t.Fatal("legacy rows must default to managed=false")
	}
	if err := db.Get(&keyValid, `SELECT template_key IS NOT NULL FROM templates WHERE name='legacy'`); err != nil {
		t.Fatalf("template_key column missing: %v", err)
	}
	if keyValid {
		t.Fatal("legacy rows must default to template_key=NULL")
	}

	// The unique index rejects duplicate non-NULL keys.
	if _, err := db.Exec(`
		INSERT INTO templates (name, subject, body, template_key, managed) VALUES ('a', '', 'b', 'dup', true);
	`); err != nil {
		t.Fatalf("first keyed insert: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO templates (name, subject, body, template_key, managed) VALUES ('c', '', 'd', 'dup', true);
	`); err == nil {
		t.Fatal("duplicate template_key must violate the partial unique index")
	}

	// Second run must be a no-op (idempotent migration).
	if err := V7_1_0(db, nil, nil, log.New(os.Stdout, "", log.LstdFlags)); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}
