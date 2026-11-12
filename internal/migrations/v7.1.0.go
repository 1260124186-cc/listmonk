package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

// V7_1_0 introduces the idempotent, provider-event-id based bounce receipt
// reconciliation tables:
//   - bounce_events: the ledger of managed provider bounce events, unique per
//     (source, event_id).
//   - bounces.bounce_event_id: 1:1 link from a managed bounce row to its event.
//   - bounce_status_ledger: ownership ledger for subscriber state changes made
//     by reconciliation so manual blocklists are never reverted.
//   - bounce_receipt_batches: idempotency records for retried batch submissions.
//
// The migration is idempotent and safe to re-run on non-final builds.
func V7_1_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bounce_events (
			id               SERIAL PRIMARY KEY,
			source           TEXT NOT NULL,
			event_id         TEXT NOT NULL,
			subscriber_id    INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE ON UPDATE CASCADE,
			campaign_id      INTEGER NULL REFERENCES campaigns(id) ON DELETE SET NULL ON UPDATE CASCADE,
			type             bounce_type NOT NULL,
			meta             JSONB NOT NULL DEFAULT '{}',
			event_created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			UNIQUE(source, event_id)
		);
		CREATE INDEX IF NOT EXISTS idx_bounce_events_sub_id ON bounce_events(subscriber_id);

		ALTER TABLE bounces ADD COLUMN IF NOT EXISTS bounce_event_id INTEGER NULL REFERENCES bounce_events(id) ON DELETE CASCADE ON UPDATE CASCADE;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_bounces_event ON bounces(bounce_event_id) WHERE bounce_event_id IS NOT NULL;

		CREATE TABLE IF NOT EXISTS bounce_status_ledger (
			subscriber_id       INTEGER PRIMARY KEY REFERENCES subscribers(id) ON DELETE CASCADE ON UPDATE CASCADE,
			prev_sub_status     subscriber_status NOT NULL,
			prev_list_statuses  JSONB NOT NULL DEFAULT '{}',
			managed_blocklist   BOOLEAN NOT NULL DEFAULT FALSE,
			managed_unsubscribe BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS bounce_receipt_batches (
			id         SERIAL PRIMARY KEY,
			source     TEXT NOT NULL,
			batch_id   TEXT NOT NULL,
			result     JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			UNIQUE(source, batch_id)
		);
	`); err != nil {
		return err
	}

	return nil
}
