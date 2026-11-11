package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

func V8_0_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	// Create the historical archive slug (alias) table. Every non-empty
	// archive_slug ever assigned to a campaign is retained here so that old
	// public archive links keep resolving to the same campaign after renames.
	// Slugs are globally unique and can only point to a single campaign.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS campaign_archive_aliases (
			id          BIGSERIAL PRIMARY KEY,
			campaign_id INTEGER NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE ON UPDATE CASCADE,
			slug        TEXT NOT NULL,
			created_at  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			updated_at  TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_camp_archive_aliases_slug ON campaign_archive_aliases(slug);
		CREATE INDEX IF NOT EXISTS idx_camp_archive_aliases_camp ON campaign_archive_aliases(campaign_id);
	`); err != nil {
		return err
	}

	// Backfill aliases from the existing canonical archive slugs. Idempotent:
	// re-running the migration skips slugs that have already been registered.
	if _, err := db.Exec(`
		INSERT INTO campaign_archive_aliases (campaign_id, slug)
		SELECT id, archive_slug FROM campaigns WHERE archive_slug IS NOT NULL
		ON CONFLICT (slug) DO NOTHING;
	`); err != nil {
		return err
	}

	return nil
}
