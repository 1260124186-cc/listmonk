package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

// V7_1_0 adds the template bundle sync metadata to templates.
// template_key is the stable identity of bundle-managed templates (NULL on
// manually created templates) and managed marks rows that a template bundle
// sync is allowed to create, update or remove.
func V7_1_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	if _, err := db.Exec(`
		ALTER TABLE templates ADD COLUMN IF NOT EXISTS template_key TEXT NULL;
		ALTER TABLE templates ADD COLUMN IF NOT EXISTS managed BOOLEAN NOT NULL DEFAULT false;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_templates_template_key ON templates (template_key) WHERE template_key IS NOT NULL;
	`); err != nil {
		return err
	}

	return nil
}
