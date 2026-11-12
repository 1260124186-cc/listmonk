package core

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/goyesql/v2"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/migrations"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v5"
)

// TestSyncBounceReceiptsIntegration exercises the full reconciliation against
// a real Postgres instance. It is skipped unless LISTMONK_TEST_DSN is set, e.g.
//
//	LISTMONK_TEST_DSN='postgres://postgres:test@localhost:55432/listmonk_test?sslmode=disable' \
//		go test ./internal/core/ -run TestSyncBounceReceiptsIntegration -v
func TestSyncBounceReceiptsIntegration(t *testing.T) {
	dsn := os.Getenv("LISTMONK_TEST_DSN")
	if dsn == "" {
		t.Skip("set LISTMONK_TEST_DSN to run the Postgres integration test")
	}

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer db.Close()

	loadSchema(t, db)

	// Re-running the migration on a schema that already has the objects must
	// be a no-op (non-final builds re-run migrations on every boot).
	if err := migrations.V7_1_0(db, nil, nil, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("re-running v7.1.0 migration: %v", err)
	}

	app := newIntegrationCore(t, db)

	t.Run("blocklist, replay and idempotent re-submission", func(t *testing.T) {
		loadSchema(t, db)
		subID, subUUID := createSubscriber(t, db, "one@example.com")
		listID := createList(t, db)
		addToList(t, db, subID, listID, "confirmed")

		batch := models.BounceReceiptBatch{
			Source:  "ses",
			BatchID: "batch-1",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-1", SubscriberUUID: subUUID, Type: models.BounceTypeHard, Meta: json.RawMessage(`{"a":1}`)},
			},
		}

		res, err := app.SyncBounceReceipts(batch)
		ok(t, err)
		equals(t, 1, res.Inserted)
		equals(t, 1, res.Blocklisted)
		equals(t, "blocklisted", subscriberStatus(t, db, subID))
		equals(t, "unsubscribed", listStatus(t, db, subID, listID))
		equals(t, 1, tableCount(t, db, "bounce_events"))
		equals(t, 1, tableCount(t, db, "bounces"))
		if !ledgerFlag(t, db, subID, "managed_blocklist") {
			t.Fatalf("expected managed_blocklist ledger flag")
		}

		// Retrying the exact same batch returns the stored result.
		replay, err := app.SyncBounceReceipts(batch)
		ok(t, err)
		if !replay.Replayed {
			t.Fatalf("expected the repeated batch to be replayed")
		}
		equals(t, 1, replay.Blocklisted)
		equals(t, 1, tableCount(t, db, "bounce_events"))

		// The same receipt without a batch id is an idempotent no-op.
		batch.BatchID = ""
		res, err = app.SyncBounceReceipts(batch)
		ok(t, err)
		equals(t, 1, res.Unchanged)
		equals(t, 0, res.Inserted)
		equals(t, 0, res.Blocklisted)
		equals(t, "blocklisted", subscriberStatus(t, db, subID))
	})

	t.Run("type correction overwrites and restores state", func(t *testing.T) {
		loadSchema(t, db)
		subID, subUUID := createSubscriber(t, db, "two@example.com")
		listID := createList(t, db)
		addToList(t, db, subID, listID, "confirmed")

		hard := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-1", SubscriberUUID: subUUID, Type: models.BounceTypeHard},
			},
		}
		_, err := app.SyncBounceReceipts(hard)
		ok(t, err)
		equals(t, "blocklisted", subscriberStatus(t, db, subID))

		// Correct the event to a soft bounce, whose threshold (2) is not met.
		soft := hard
		soft.Events[0].Type = models.BounceTypeSoft
		res, err := app.SyncBounceReceipts(soft)
		ok(t, err)
		equals(t, 1, res.Updated)
		equals(t, 1, res.Reenabled)
		equals(t, "enabled", subscriberStatus(t, db, subID))
		equals(t, "confirmed", listStatus(t, db, subID, listID))
		equals(t, 0, tableCountWhere(t, db, "bounce_status_ledger", "subscriber_id", subID))

		var typ string
		if err := db.Get(&typ, `SELECT type FROM bounce_events WHERE event_id='evt-1'`); err != nil {
			t.Fatalf("reading corrected event: %v", err)
		}
		equals(t, models.BounceTypeSoft, typ)
	})

	t.Run("events absent from the latest receipt are retracted", func(t *testing.T) {
		loadSchema(t, db)
		subID, subUUID := createSubscriber(t, db, "three@example.com")
		listID := createList(t, db)
		addToList(t, db, subID, listID, "confirmed")

		first := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-1", SubscriberUUID: subUUID, Type: models.BounceTypeHard},
				{EventID: "evt-2", SubscriberUUID: subUUID, Type: models.BounceTypeHard},
			},
		}
		_, err := app.SyncBounceReceipts(first)
		ok(t, err)
		equals(t, 2, tableCount(t, db, "bounce_events"))

		// Latest receipt only carries evt-2; evt-1 must be withdrawn.
		second := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-2", SubscriberUUID: subUUID, Type: models.BounceTypeHard},
			},
		}
		res, err := app.SyncBounceReceipts(second)
		ok(t, err)
		equals(t, 1, res.Retracted)
		equals(t, 1, tableCount(t, db, "bounce_events"))
		equals(t, 1, tableCount(t, db, "bounces"))
		// One hard bounce still justifies the blocklist.
		equals(t, "blocklisted", subscriberStatus(t, db, subID))

		// An empty snapshot retracts everything and restores the subscriber.
		res, err = app.SyncBounceReceipts(models.BounceReceiptBatch{Source: "ses"})
		ok(t, err)
		equals(t, 1, res.Retracted)
		equals(t, 1, res.Reenabled)
		equals(t, 0, tableCount(t, db, "bounce_events"))
		equals(t, 0, tableCount(t, db, "bounces"))
		equals(t, "enabled", subscriberStatus(t, db, subID))
		equals(t, "confirmed", listStatus(t, db, subID, listID))
	})

	t.Run("manual blocklist and manual list edits are never reverted", func(t *testing.T) {
		loadSchema(t, db)
		subID, subUUID := createSubscriber(t, db, "four@example.com")
		listA, listB := createList(t, db), createList(t, db)
		addToList(t, db, subID, listA, "confirmed")
		addToList(t, db, subID, listB, "confirmed")

		_, err := app.SyncBounceReceipts(models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-1", SubscriberUUID: subUUID, Type: models.BounceTypeHard},
			},
		})
		ok(t, err)
		equals(t, "blocklisted", subscriberStatus(t, db, subID))

		// Admin re-affirms the blocklist and edits list B after the system write.
		if _, err := db.Exec(`UPDATE subscribers SET status='blocklisted', updated_at=NOW() + INTERVAL '1 minute' WHERE id=$1`, subID); err != nil {
			t.Fatalf("manual blocklist update: %v", err)
		}
		if _, err := db.Exec(`UPDATE subscriber_lists SET updated_at=NOW() + INTERVAL '1 minute' WHERE subscriber_id=$1 AND list_id=$2`, subID, listB); err != nil {
			t.Fatalf("manual list update: %v", err)
		}

		// Retract the event: the manual blocklist and list B must survive.
		_, err = app.SyncBounceReceipts(models.BounceReceiptBatch{Source: "ses"})
		ok(t, err)
		equals(t, "blocklisted", subscriberStatus(t, db, subID))
		equals(t, "unsubscribed", listStatus(t, db, subID, listB))
		// List A was last written by the reconciliation and is restored.
		equals(t, "confirmed", listStatus(t, db, subID, listA))
		equals(t, 0, tableCountWhere(t, db, "bounce_status_ledger", "subscriber_id", subID))

		// A subscriber manually blocklisted without any ledger is never claimed.
		manualID, manualUUID := createSubscriber(t, db, "manual@example.com")
		if _, err := db.Exec(`UPDATE subscribers SET status='blocklisted' WHERE id=$1`, manualID); err != nil {
			t.Fatalf("manual blocklist: %v", err)
		}
		_, err = app.SyncBounceReceipts(models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-9", SubscriberUUID: manualUUID, Type: models.BounceTypeHard},
			},
		})
		ok(t, err)
		equals(t, "blocklisted", subscriberStatus(t, db, manualID))
		equals(t, 0, tableCountWhere(t, db, "bounce_status_ledger", "subscriber_id", manualID))
	})

	t.Run("unknown subscriber and payload conflicts reject the whole batch", func(t *testing.T) {
		loadSchema(t, db)
		_, knownUUID := createSubscriber(t, db, "known@example.com")

		_, err := app.SyncBounceReceipts(models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-ok", SubscriberUUID: knownUUID, Type: models.BounceTypeHard},
				{EventID: "evt-bad", Email: "ghost@example.com", Type: models.BounceTypeHard},
			},
		})
		httpErr := expectHTTPError(t, err, 400)
		if !contains(httpErr.Message, "evt-bad") {
			t.Fatalf("expected error to name evt-bad, got: %v", httpErr.Message)
		}
		equals(t, 0, tableCount(t, db, "bounce_events"))
		equals(t, 0, tableCount(t, db, "bounces"))

		_, err = app.SyncBounceReceipts(models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "evt-dup", SubscriberUUID: knownUUID, Type: models.BounceTypeHard},
				{EventID: "evt-dup", SubscriberUUID: knownUUID, Type: models.BounceTypeSoft},
			},
		})
		httpErr = expectHTTPError(t, err, 400)
		if !contains(httpErr.Message, "evt-dup") {
			t.Fatalf("expected error to name evt-dup, got: %v", httpErr.Message)
		}
		equals(t, 0, tableCount(t, db, "bounce_events"))
	})

	t.Run("out-of-order and late delivery converge on the latest snapshot", func(t *testing.T) {
		// Reference path: the latest snapshot applied on a clean database.
		loadSchema(t, db)
		_, refA := createSubscriber(t, db, "a@example.com")
		_, refB := createSubscriber(t, db, "b@example.com")
		latest := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "e1", SubscriberUUID: refA, Type: models.BounceTypeSoft},
				{EventID: "e2", SubscriberUUID: refB, Type: models.BounceTypeHard},
			},
		}
		_, err := app.SyncBounceReceipts(latest)
		ok(t, err)
		reference := snapshotWorld(t, db)

		// Chaotic path: older snapshot first, then latest, a retried latest,
		// a stale snapshot arriving late, and finally latest again.
		loadSchema(t, db)
		_, uuidA := createSubscriber(t, db, "a@example.com")
		_, uuidB := createSubscriber(t, db, "b@example.com")
		old := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "e1", SubscriberUUID: uuidA, Type: models.BounceTypeHard},
			},
		}
		latest.Events[0].SubscriberUUID = uuidA
		latest.Events[1].SubscriberUUID = uuidB

		_, err = app.SyncBounceReceipts(old)
		ok(t, err)
		_, err = app.SyncBounceReceipts(latest)
		ok(t, err)

		// Immediate retry of the latest snapshot must not move anything.
		retry, err := app.SyncBounceReceipts(latest)
		ok(t, err)
		equals(t, 0, retry.Inserted)
		equals(t, 0, retry.Updated)
		equals(t, 0, retry.Retracted)

		// A stale snapshot arrives late and temporarily wins, then the latest
		// snapshot is re-delivered and must converge back to the reference.
		_, err = app.SyncBounceReceipts(old)
		ok(t, err)
		_, err = app.SyncBounceReceipts(latest)
		ok(t, err)

		if !reflect.DeepEqual(reference, snapshotWorld(t, db)) {
			t.Fatalf("chaotic delivery did not converge:\nreference: %+v\nactual:    %+v",
				reference, snapshotWorld(t, db))
		}
	})

	t.Run("re-applying the latest snapshot is a fixed point", func(t *testing.T) {
		loadSchema(t, db)
		_, uuidA := createSubscriber(t, db, "a@example.com")
		_, uuidB := createSubscriber(t, db, "b@example.com")

		latest := models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "e1", SubscriberUUID: uuidA, Type: models.BounceTypeHard},
				{EventID: "e2", SubscriberUUID: uuidB, Type: models.BounceTypeSoft},
			},
		}
		// History before the latest snapshot must not affect the fixed point.
		_, err := app.SyncBounceReceipts(models.BounceReceiptBatch{
			Source: "ses",
			Events: []models.BounceReceiptEvent{
				{EventID: "e0", SubscriberUUID: uuidA, Type: models.BounceTypeSoft},
			},
		})
		ok(t, err)
		_, err = app.SyncBounceReceipts(latest)
		ok(t, err)
		first := snapshotWorld(t, db)

		// Re-submit the same snapshot repeatedly; nothing should move.
		for i := 0; i < 2; i++ {
			res, err := app.SyncBounceReceipts(latest)
			ok(t, err)
			equals(t, 0, res.Inserted)
			equals(t, 0, res.Updated)
			equals(t, 0, res.Retracted)
			equals(t, 0, res.Blocklisted)
			equals(t, 0, res.Reenabled)
		}
		if !reflect.DeepEqual(first, snapshotWorld(t, db)) {
			t.Fatalf("re-applying the latest snapshot changed the state")
		}
	})
}

// --- fixtures and helpers -------------------------------------------------

func newIntegrationCore(t *testing.T, db *sqlx.DB) *Core {
	t.Helper()

	queryBytes, err := os.ReadFile("../../queries/bounces.sql")
	if err != nil {
		t.Fatalf("reading queries: %v", err)
	}
	parsed, err := goyesql.ParseBytes(queryBytes)
	if err != nil {
		t.Fatalf("parsing queries: %v", err)
	}

	q := &models.Queries{}
	rv := reflect.ValueOf(q).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("query")
		if tag == "" || rv.Field(i).Kind() != reflect.String {
			continue
		}
		if query, ok := parsed[tag]; ok {
			rv.Field(i).SetString(query.Query)
		}
	}

	langBytes, err := os.ReadFile("../../i18n/en.json")
	if err != nil {
		t.Fatalf("reading i18n bundle: %v", err)
	}
	i18nInstance, err := i18n.New(langBytes)
	if err != nil {
		t.Fatalf("loading i18n: %v", err)
	}

	return New(&Opt{
		Constants: Constants{
			BounceActions: map[string]struct {
				Count  int
				Action string
			}{
				models.BounceTypeSoft:      {Count: 2, Action: "none"},
				models.BounceTypeHard:      {Count: 1, Action: "blocklist"},
				models.BounceTypeComplaint: {Count: 1, Action: "blocklist"},
			},
		},
		I18n:    i18nInstance,
		DB:      db,
		Queries: q,
		Log:     log.New(io.Discard, "", 0),
	}, nil)
}

func loadSchema(t *testing.T, db *sqlx.DB) {
	t.Helper()
	b, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatalf("reading schema: %v", err)
	}
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatalf("loading schema: %v", err)
	}
}

func createSubscriber(t *testing.T, db *sqlx.DB, email string) (int, string) {
	t.Helper()
	u := uuid.NewString()
	var id int
	if err := db.QueryRow(
		`INSERT INTO subscribers (uuid, email, name) VALUES ($1, $2, 't') RETURNING id`,
		u, email).Scan(&id); err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	return id, u
}

func createList(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var id int
	if err := db.QueryRow(
		`INSERT INTO lists (uuid, name, type) VALUES ($1, 'l', 'public') RETURNING id`,
		uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("creating list: %v", err)
	}
	return id
}

func addToList(t *testing.T, db *sqlx.DB, subID, listID int, status string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO subscriber_lists (subscriber_id, list_id, status) VALUES ($1, $2, $3::subscription_status)`,
		subID, listID, status); err != nil {
		t.Fatalf("adding subscriber to list: %v", err)
	}
}

func subscriberStatus(t *testing.T, db *sqlx.DB, subID int) string {
	t.Helper()
	var status string
	if err := db.Get(&status, `SELECT status FROM subscribers WHERE id=$1`, subID); err != nil {
		t.Fatalf("reading subscriber status: %v", err)
	}
	return status
}

func listStatus(t *testing.T, db *sqlx.DB, subID, listID int) string {
	t.Helper()
	var status string
	if err := db.Get(&status,
		`SELECT status FROM subscriber_lists WHERE subscriber_id=$1 AND list_id=$2`, subID, listID); err != nil {
		t.Fatalf("reading list status: %v", err)
	}
	return status
}

func tableCount(t *testing.T, db *sqlx.DB, table string) int {
	t.Helper()
	return tableCountWhere(t, db, table, "", nil)
}

func tableCountWhere(t *testing.T, db *sqlx.DB, table, col string, val any) int {
	t.Helper()
	var n int
	var err error
	if col == "" {
		err = db.Get(&n, "SELECT COUNT(*) FROM "+table)
	} else {
		err = db.Get(&n, "SELECT COUNT(*) FROM "+table+" WHERE "+col+"=$1", val)
	}
	if err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func ledgerFlag(t *testing.T, db *sqlx.DB, subID int, flag string) bool {
	t.Helper()
	var v bool
	if err := db.Get(&v,
		"SELECT "+flag+" FROM bounce_status_ledger WHERE subscriber_id=$1", subID); err != nil {
		t.Fatalf("reading ledger flag %s: %v", flag, err)
	}
	return v
}

// worldState is the order-independent representation used to prove that
// different delivery orders converge to identical final state.
type worldState struct {
	Events     []string // event_id:type:subscriber_email
	Bounces    []string // type:source
	Subs       []string // email:status
	Lists      []string // subscriber_email:list_id:status
	LedgerRows int
}

func snapshotWorld(t *testing.T, db *sqlx.DB) worldState {
	t.Helper()
	var w worldState

	if err := db.Select(&w.Events, `
		SELECT e.event_id || ':' || e.type || ':' || s.email
		FROM bounce_events e JOIN subscribers s ON s.id = e.subscriber_id
		ORDER BY 1`); err != nil {
		t.Fatalf("snapshotting events: %v", err)
	}
	if err := db.Select(&w.Bounces, `SELECT type || ':' || source FROM bounces ORDER BY 1`); err != nil {
		t.Fatalf("snapshotting bounces: %v", err)
	}
	if err := db.Select(&w.Subs, `SELECT email || ':' || status FROM subscribers ORDER BY 1`); err != nil {
		t.Fatalf("snapshotting subscribers: %v", err)
	}
	if err := db.Select(&w.Lists, `
		SELECT s.email || ':' || sl.list_id || ':' || sl.status
		FROM subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id
		ORDER BY 1`); err != nil {
		t.Fatalf("snapshotting list statuses: %v", err)
	}
	if err := db.Get(&w.LedgerRows, `SELECT COUNT(*) FROM bounce_status_ledger`); err != nil {
		t.Fatalf("snapshotting ledger: %v", err)
	}

	sort.Strings(w.Events)
	sort.Strings(w.Bounces)
	sort.Strings(w.Subs)
	sort.Strings(w.Lists)
	return w
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func equals[T comparable](t *testing.T, want, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func expectHTTPError(t *testing.T, err error, code int) *echo.HTTPError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected HTTP %d error, got nil", code)
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected echo.HTTPError, got %T: %v", err, err)
	}
	if httpErr.Code != code {
		t.Fatalf("expected HTTP %d, got %d (%v)", code, httpErr.Code, httpErr.Message)
	}
	return httpErr
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
