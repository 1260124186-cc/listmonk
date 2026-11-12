package core

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v5"
	"github.com/lib/pq"
)

// Bounce actions configured through bounce.actions. The "delete" action is
// coerced to "blocklist" by the reconciliation because a deleted subscriber
// cannot be restored when an event is later corrected or retracted.
const (
	bounceActionNone        = "none"
	bounceActionUnsubscribe = "unsubscribe"
	bounceActionBlocklist   = "blocklist"
	bounceActionDelete      = "delete"
)

const (
	maxReceiptSourceLen = 255
	maxReceiptEventLen  = 255
)

var reReceiptUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// receiptValidationErrorKind enumerates the batch-level validation failures.
// The whole batch is rejected and the offending provider event IDs are always
// surfaced in the response.
type receiptValidationErrorKind string

const (
	receiptErrSource       receiptValidationErrorKind = "source"
	receiptErrEventID      receiptValidationErrorKind = "event_id"
	receiptErrTarget       receiptValidationErrorKind = "target"
	receiptErrType         receiptValidationErrorKind = "type"
	receiptErrEmail        receiptValidationErrorKind = "email"
	receiptErrUUID         receiptValidationErrorKind = "uuid"
	receiptErrCampaignUUID receiptValidationErrorKind = "campaign_uuid"
	receiptErrConflict     receiptValidationErrorKind = "conflict"
)

// receiptValidationError describes a single failure while validating a batch.
type receiptValidationError struct {
	Kind    receiptValidationErrorKind
	EventID string
	Value   string
	Index   int
}

func (e *receiptValidationError) Error() string {
	return "invalid bounce receipt batch: " + string(e.Kind) + " for event " + e.EventID
}

// bounceEventRow is the stored representation of a managed provider event.
type bounceEventRow struct {
	ID             int             `db:"id"`
	SubscriberID   int             `db:"subscriber_id"`
	CampaignID     sql.NullInt64   `db:"campaign_id"`
	Type           string          `db:"type"`
	Meta           json.RawMessage `db:"meta"`
	EventCreatedAt time.Time       `db:"event_created_at"`
}

// bounceStatusLedgerRow mirrors the bounce_status_ledger ownership table.
type bounceStatusLedgerRow struct {
	SubscriberID       int             `db:"subscriber_id"`
	PrevSubStatus      string          `db:"prev_sub_status"`
	PrevListStatuses   json.RawMessage `db:"prev_list_statuses"`
	ManagedBlocklist   bool            `db:"managed_blocklist"`
	ManagedUnsubscribe bool            `db:"managed_unsubscribe"`
	UpdatedAt          time.Time       `db:"updated_at"`
}

// reconcileOutcome accumulates the subscriber state changes made for one
// subscriber during a reconciliation pass.
type reconcileOutcome struct {
	blocklisted  int
	unsubscribed int
	reenabled    int
}

// validateBounceReceiptBatch normalizes and validates a receipt batch without
// touching the database. Duplicate event IDs with identical payloads are
// de-duplicated; duplicate event IDs with conflicting payloads, invalid types,
// targets or UUIDs fail the entire batch. It is a free function so the rules
// can be unit tested without a database.
func validateBounceReceiptBatch(batch models.BounceReceiptBatch) (string, []models.BounceReceiptEvent, *receiptValidationError) {
	source := strings.TrimSpace(batch.Source)
	if source == "" || len(source) > maxReceiptSourceLen {
		return "", nil, &receiptValidationError{Kind: receiptErrSource}
	}

	events := make([]models.BounceReceiptEvent, 0, len(batch.Events))
	seen := make(map[string]int, len(batch.Events))

	for i, ev := range batch.Events {
		ev.EventID = strings.TrimSpace(ev.EventID)
		if ev.EventID == "" || len(ev.EventID) > maxReceiptEventLen {
			return source, nil, &receiptValidationError{Kind: receiptErrEventID, Index: i}
		}

		if ev.Type != models.BounceTypeHard && ev.Type != models.BounceTypeSoft && ev.Type != models.BounceTypeComplaint {
			return source, nil, &receiptValidationError{Kind: receiptErrType, EventID: ev.EventID, Value: ev.Type}
		}

		ev.SubscriberUUID = strings.TrimSpace(ev.SubscriberUUID)
		ev.Email = strings.TrimSpace(ev.Email)
		if ev.Email == "" && ev.SubscriberUUID == "" {
			return source, nil, &receiptValidationError{Kind: receiptErrTarget, EventID: ev.EventID}
		}
		if ev.SubscriberUUID != "" && !reReceiptUUID.MatchString(ev.SubscriberUUID) {
			return source, nil, &receiptValidationError{Kind: receiptErrUUID, EventID: ev.EventID}
		}
		if ev.Email != "" {
			em, err := utils.SanitizeEmail(ev.Email)
			if err != nil {
				return source, nil, &receiptValidationError{Kind: receiptErrEmail, EventID: ev.EventID}
			}
			ev.Email = em
		}

		ev.CampaignUUID = strings.TrimSpace(ev.CampaignUUID)
		if ev.CampaignUUID != "" && !reReceiptUUID.MatchString(ev.CampaignUUID) {
			return source, nil, &receiptValidationError{Kind: receiptErrCampaignUUID, EventID: ev.EventID}
		}

		// Truncate an explicit provider timestamp to microsecond precision to
		// match the timestamptz storage round-trip. A zero timestamp is left
		// untouched so the SQL layer can preserve the stored value on retries.
		if !ev.CreatedAt.IsZero() {
			ev.CreatedAt = ev.CreatedAt.UTC().Truncate(time.Microsecond)
		}
		if len(ev.Meta) == 0 {
			ev.Meta = json.RawMessage(`{}`)
		}

		signature := ev.Type + "|" + ev.SubscriberUUID + "|" + ev.Email + "|" + ev.CampaignUUID
		if firstIdx, ok := seen[ev.EventID]; ok {
			if events[firstIdx].Type+"|"+events[firstIdx].SubscriberUUID+"|"+events[firstIdx].Email+"|"+events[firstIdx].CampaignUUID != signature {
				return source, nil, &receiptValidationError{Kind: receiptErrConflict, EventID: ev.EventID}
			}
			// Identical duplicate within the batch: collapse it.
			continue
		}

		seen[ev.EventID] = len(events)
		events = append(events, ev)
	}

	return source, events, nil
}

// SyncBounceReceipts applies a full snapshot batch of provider bounce receipts
// idempotently inside a single transaction:
//
//   - Re-submitted (source, event_id) events are counted once and type/meta
//     corrections overwrite the stored value.
//   - Managed events of the source that are absent from the batch are retracted
//     together with their bounce rows.
//   - Subscriber blocklist/unsubscribe state is recomputed from the resulting
//     bounce counts; state the reconciliation does not own (manual blocklists)
//     is never modified.
//   - A repeated (source, batch_id) submission replays the stored result.
//
// Any unknown subscriber or payload conflict rejects the entire batch and the
// response names the offending provider event IDs.
func (c *Core) SyncBounceReceipts(batch models.BounceReceiptBatch) (models.BounceReceiptResult, error) {
	source, events, verr := validateBounceReceiptBatch(batch)
	if verr != nil {
		return models.BounceReceiptResult{}, c.receiptValidationHTTPError(verr)
	}

	res := models.BounceReceiptResult{
		Source:   source,
		BatchID:  strings.TrimSpace(batch.BatchID),
		Received: len(events),
	}

	tx, err := c.db.Beginx()
	if err != nil {
		return res, c.receiptInternalError("error beginning bounce receipt transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize concurrent batches of the same source so retries and
	// out-of-order deliveries converge to the same final state.
	if _, err := tx.Exec(c.q.LockBounceSource, source); err != nil {
		return res, c.receiptInternalError("error locking bounce source", err)
	}

	// Replay a previously processed batch without touching any data.
	if res.BatchID != "" {
		var raw json.RawMessage
		if err := tx.Get(&raw, c.q.GetBounceReceiptBatch, source, res.BatchID); err == nil {
			var prev models.BounceReceiptResult
			if err := json.Unmarshal(raw, &prev); err == nil {
				prev.Replayed = true
				if err := tx.Commit(); err != nil {
					return res, c.receiptInternalError("error committing replayed bounce receipt batch", err)
				}
				return prev, nil
			}
			c.log.Printf("error decoding stored bounce receipt batch result: %v", err)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return res, c.receiptInternalError("error looking up bounce receipt batch", err)
		}
	}

	// Resolve every subscriber and campaign referenced by the batch up-front.
	subByEmail, subByUUID, err := c.resolveReceiptSubscribers(tx, events)
	if err != nil {
		return res, err
	}
	campByUUID, err := c.resolveReceiptCampaigns(tx, events)
	if err != nil {
		return res, err
	}

	// Whole-batch rejection on unknown subscribers; collect every event ID.
	unknown := make([]string, 0)
	for _, ev := range events {
		if ev.SubscriberUUID != "" {
			if _, ok := subByUUID[ev.SubscriberUUID]; !ok {
				unknown = append(unknown, ev.EventID)
			}
			continue
		}
		if _, ok := subByEmail[ev.Email]; !ok {
			unknown = append(unknown, ev.EventID)
		}
	}
	if len(unknown) > 0 {
		return res, echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncUnknownSubscriber", "ids", strings.Join(unknown, ", ")))
	}

	affected := make(map[int]struct{})
	incomingIDs := make([]string, 0, len(events))

	// Upsert every event and mirror it into the bounces table 1:1.
	for _, ev := range events {
		incomingIDs = append(incomingIDs, ev.EventID)

		subID := subByEmail[ev.Email]
		if ev.SubscriberUUID != "" {
			subID = subByUUID[ev.SubscriberUUID]
		}
		campID := sql.NullInt64{}
		if ev.CampaignUUID != "" {
			if id, ok := campByUUID[ev.CampaignUUID]; ok {
				campID = sql.NullInt64{Int64: int64(id), Valid: true}
			}
		}

		// A zero timestamp is sent as SQL NULL so the stored value is kept on
		// conflict instead of being replaced by the receive time.
		var created sql.NullTime
		if !ev.CreatedAt.IsZero() {
			created = sql.NullTime{Time: ev.CreatedAt, Valid: true}
		}

		var existing bounceEventRow
		err := tx.Get(&existing, c.q.GetBounceEvent, source, ev.EventID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res.Inserted++
		case err != nil:
			return res, c.receiptInternalError("error fetching bounce event", err)
		default:
			if eventEqualsReceipt(existing, subID, campID, created, ev) {
				res.Unchanged++
			} else {
				res.Updated++
			}
			// A corrected event may point at a different subscriber; the old
			// subscriber must also be reconciled.
			if existing.SubscriberID != subID {
				affected[existing.SubscriberID] = struct{}{}
			}
		}

		var eventID int
		if err := tx.Get(&eventID, c.q.UpsertBounceEvent,
			source, ev.EventID, subID, campID, ev.Type, []byte(ev.Meta), created); err != nil {
			return res, c.receiptInternalError("error upserting bounce event", err)
		}

		if _, err := tx.Exec(c.q.UpsertManagedBounce,
			subID, campID, ev.Type, source, []byte(ev.Meta), created, eventID); err != nil {
			return res, c.receiptInternalError("error upserting managed bounce", err)
		}

		affected[subID] = struct{}{}
	}

	// Retract managed events that the latest receipt no longer contains.
	var retracting []struct {
		ID           int `db:"id"`
		SubscriberID int `db:"subscriber_id"`
	}
	if err := tx.Select(&retracting, c.q.GetRetractingBounceEvents, source, pq.Array(incomingIDs)); err != nil {
		return res, c.receiptInternalError("error fetching retracting bounce events", err)
	}
	if len(retracting) > 0 {
		retractIDs := make([]int, 0, len(retracting))
		for _, r := range retracting {
			retractIDs = append(retractIDs, r.ID)
			affected[r.SubscriberID] = struct{}{}
		}
		if _, err := tx.Exec(c.q.DeleteBounceEvents, source, pq.Array(retractIDs)); err != nil {
			return res, c.receiptInternalError("error retracting bounce events", err)
		}
		res.Retracted = len(retractIDs)
	}

	// Recompute subscriber state deterministically regardless of delivery order.
	subIDs := make([]int, 0, len(affected))
	for id := range affected {
		subIDs = append(subIDs, id)
	}
	sort.Ints(subIDs)

	for _, subID := range subIDs {
		out, err := c.reconcileBounceSubscriber(tx, subID)
		if err != nil {
			return res, err
		}
		res.Blocklisted += out.blocklisted
		res.Unsubscribed += out.unsubscribed
		res.Reenabled += out.reenabled
	}

	// Record the batch result so an identical retry becomes a replay.
	if res.BatchID != "" {
		raw, err := json.Marshal(res)
		if err != nil {
			return res, c.receiptInternalError("error encoding bounce receipt batch result", err)
		}
		if _, err := tx.Exec(c.q.InsertBounceReceiptBatch, source, res.BatchID, string(raw)); err != nil {
			return res, c.receiptInternalError("error recording bounce receipt batch", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return res, c.receiptInternalError("error committing bounce receipt batch", err)
	}

	return res, nil
}

// reconcileBounceSubscriber recomputes one subscriber's blocklist / list
// subscription state from the current bounce rows and the configured actions.
// State changes are recorded in bounce_status_ledger and only state owned by
// the reconciliation (untouched since it was applied) is ever reverted.
func (c *Core) reconcileBounceSubscriber(tx *sqlx.Tx, subID int) (reconcileOutcome, error) {
	var out reconcileOutcome

	var sub struct {
		ID        int       `db:"id"`
		Status    string    `db:"status"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	if err := tx.Get(&sub, c.q.GetSubscriberForBounceReconcile, subID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Subscriber deleted concurrently; its bounce rows and ledger
			// cascade away, so there is nothing to reconcile.
			return out, nil
		}
		return out, c.receiptInternalError("error locking subscriber for bounce reconciliation", err)
	}

	var rows []struct {
		Type string `db:"type"`
		Num  int    `db:"num"`
	}
	if err := tx.Select(&rows, c.q.GetBounceCounts, subID); err != nil {
		return out, c.receiptInternalError("error counting subscriber bounces", err)
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Type] = r.Num
	}
	target := desiredBounceAction(counts, c.consts.BounceActions)

	var (
		ledger    bounceStatusLedgerRow
		hasLedger bool
		// snapStatus/snapLists hold the snapshot captured during this run when
		// no ledger row existed, so the final upsert never writes zero values.
		snapStatus string
		snapLists  json.RawMessage
		err        error
	)
	if err := tx.Get(&ledger, c.q.GetBounceStatusLedger, subID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, c.receiptInternalError("error fetching bounce status ledger", err)
		}
	} else {
		hasLedger = true
	}

	blocklistOwned := hasLedger && ledger.ManagedBlocklist
	unsubOwned := hasLedger && ledger.ManagedUnsubscribe
	wasBlocklistOwned := blocklistOwned

	// Phase 1: relinquish a reconciliation-owned blocklist that the current
	// counts no longer justify. The updated_at guard leaves manual blocklists
	// (and any later manual edit) untouched.
	reenabled := false
	if blocklistOwned && target != bounceActionBlocklist {
		res, err := tx.Exec(c.q.RestoreBounceSubscriberStatus, subID, ledger.PrevSubStatus, ledger.UpdatedAt)
		if err != nil {
			return out, c.receiptInternalError("error restoring subscriber status", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			reenabled = true
			out.reenabled++
			sub.Status = ledger.PrevSubStatus
		}
		// Ownership is released either way: if the row could not be restored,
		// a manual intervention now owns the blocklisted state.
		blocklistOwned = false
	}

	// Phase 2: reconcile per-list subscription statuses.
	switch target {
	case bounceActionNone:
		// Restore list rows the reconciliation last wrote. Rows changed later
		// (manual unsubscribes) fail the updated_at guard and are preserved.
		if hasLedger && (wasBlocklistOwned || unsubOwned) {
			if _, err := tx.Exec(c.q.RestoreBounceListStatuses, subID, []byte(ledger.PrevListStatuses), ledger.UpdatedAt); err != nil {
				return out, c.receiptInternalError("error restoring list statuses", err)
			}
		}
		unsubOwned = false

	case bounceActionUnsubscribe:
		// A blocklisted subscriber (manual or unreverted) already has every
		// list unsubscribed; do not claim ownership of that state.
		if sub.Status != models.SubscriberStatusBlockListed {
			if !hasLedger {
				snapStatus, snapLists, err = c.createLedgerSnapshot(tx, subID, sub.Status)
				if err != nil {
					return out, err
				}
				hasLedger = true
			}
			res, err := tx.Exec(c.q.ApplyBounceUnsubscribeLists, subID)
			if err != nil {
				return out, c.receiptInternalError("error unsubscribing subscriber lists", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				out.unsubscribed++
			}
			unsubOwned = true
		} else if reenabled {
			// The blocklist was reverted this run; its list unsubscribes
			// remain in effect and stay reconciliation-owned.
			unsubOwned = true
		}
	}

	// Phase 3: apply a blocklist the counts now require. An already
	// blocklisted subscriber without a ledger is manually blocklisted and
	// must never be claimed.
	if target == bounceActionBlocklist && sub.Status != models.SubscriberStatusBlockListed {
		if !hasLedger {
			snapStatus, snapLists, err = c.createLedgerSnapshot(tx, subID, sub.Status)
			if err != nil {
				return out, err
			}
			hasLedger = true
		}
		if _, err := tx.Exec(c.q.ApplyBounceBlocklistSubscriber, subID); err != nil {
			return out, c.receiptInternalError("error blocklisting subscriber", err)
		}
		if _, err := tx.Exec(c.q.ApplyBounceBlocklistLists, subID); err != nil {
			return out, c.receiptInternalError("error unsubscribing blocklisted subscriber lists", err)
		}
		out.blocklisted++
		blocklistOwned = true
		unsubOwned = true
	}

	// Persist or discard the ownership ledger.
	if blocklistOwned || unsubOwned {
		prevStatus := ledger.PrevSubStatus
		prevLists := ledger.PrevListStatuses
		// A ledger row created during this run carries the fresh snapshot.
		if snapStatus != "" {
			prevStatus = snapStatus
			prevLists = snapLists
		}
		if _, err := tx.Exec(c.q.UpsertBounceStatusLedger,
			subID, prevStatus, []byte(prevLists), blocklistOwned, unsubOwned); err != nil {
			return out, c.receiptInternalError("error upserting bounce status ledger", err)
		}
	} else if hasLedger {
		if _, err := tx.Exec(c.q.DeleteBounceStatusLedger, subID); err != nil {
			return out, c.receiptInternalError("error deleting bounce status ledger", err)
		}
	}

	return out, nil
}

// createLedgerSnapshot inserts the initial ownership row capturing the
// subscriber's current status and per-list subscription statuses before the
// reconciliation modifies anything, and returns the captured snapshot.
func (c *Core) createLedgerSnapshot(tx *sqlx.Tx, subID int, curStatus string) (string, json.RawMessage, error) {
	var statuses json.RawMessage
	if err := tx.Get(&statuses, c.q.SnapshotBounceListStatuses, subID); err != nil {
		return "", nil, c.receiptInternalError("error snapshotting subscriber list statuses", err)
	}
	if _, err := tx.Exec(c.q.UpsertBounceStatusLedger,
		subID, curStatus, []byte(statuses), false, false); err != nil {
		return "", nil, c.receiptInternalError("error creating bounce status ledger", err)
	}
	return curStatus, statuses, nil
}

// desiredBounceAction resolves the strongest action whose per-type bounce
// threshold is satisfied by the current counts.
func desiredBounceAction(counts map[string]int, actions map[string]struct {
	Count  int
	Action string
}) string {
	rank := map[string]int{
		bounceActionNone:        0,
		bounceActionUnsubscribe: 1,
		bounceActionBlocklist:   2,
		bounceActionDelete:      2,
	}
	best := bounceActionNone
	for typ, num := range counts {
		cfg, ok := actions[typ]
		if !ok || num < cfg.Count {
			continue
		}
		action := cfg.Action
		// A hard delete is irreversible and incompatible with retraction, so
		// the reconciliation downgrades it to a reversible blocklist.
		if action == bounceActionDelete {
			action = bounceActionBlocklist
		}
		if rank[action] > rank[best] {
			best = action
		}
	}
	return best
}

// eventEqualsReceipt reports whether the stored event already reflects the
// incoming receipt so retries do not register as corrections. A NULL incoming
// timestamp means the provider omitted it and must not count as a change.
func eventEqualsReceipt(row bounceEventRow, subID int, campID sql.NullInt64, created sql.NullTime, ev models.BounceReceiptEvent) bool {
	if row.SubscriberID != subID || row.Type != ev.Type {
		return false
	}
	if created.Valid && !row.EventCreatedAt.UTC().Equal(created.Time.UTC()) {
		return false
	}
	if (row.CampaignID.Valid != campID.Valid) || (row.CampaignID.Valid && row.CampaignID.Int64 != campID.Int64) {
		return false
	}
	var stored, incoming bytes.Buffer
	if err := json.Compact(&stored, row.Meta); err != nil {
		return false
	}
	if err := json.Compact(&incoming, ev.Meta); err != nil {
		return false
	}
	return bytes.Equal(stored.Bytes(), incoming.Bytes())
}

// resolveReceiptSubscribers loads every subscriber referenced in the batch in
// a single query and returns email/uuid to ID maps.
func (c *Core) resolveReceiptSubscribers(tx *sqlx.Tx, events []models.BounceReceiptEvent) (map[string]int, map[string]int, error) {
	emails := make([]string, 0, len(events))
	uuids := make([]string, 0, len(events))
	seen := make(map[string]struct{})
	for _, ev := range events {
		if ev.Email != "" {
			if _, ok := seen[ev.Email]; !ok {
				seen[ev.Email] = struct{}{}
				emails = append(emails, ev.Email)
			}
		}
		if ev.SubscriberUUID != "" {
			if _, ok := seen[ev.SubscriberUUID]; !ok {
				seen[ev.SubscriberUUID] = struct{}{}
				uuids = append(uuids, ev.SubscriberUUID)
			}
		}
	}

	rows := make([]struct {
		ID    int    `db:"id"`
		Email string `db:"email"`
		UUID  string `db:"uuid"`
	}, 0, len(events))
	if err := tx.Select(&rows, c.q.ResolveBounceSubscribers, pq.Array(emails), pq.Array(uuids)); err != nil {
		return nil, nil, c.receiptInternalError("error resolving bounce receipt subscribers", err)
	}

	byEmail := make(map[string]int, len(rows))
	byUUID := make(map[string]int, len(rows))
	for _, r := range rows {
		byEmail[r.Email] = r.ID
		byUUID[r.UUID] = r.ID
	}
	return byEmail, byUUID, nil
}

// resolveReceiptCampaigns loads the campaigns referenced in the batch. Unknown
// campaign UUIDs resolve to NULL, matching the legacy record-bounce behaviour.
func (c *Core) resolveReceiptCampaigns(tx *sqlx.Tx, events []models.BounceReceiptEvent) (map[string]int, error) {
	uuids := make([]string, 0, len(events))
	seen := make(map[string]struct{})
	for _, ev := range events {
		if ev.CampaignUUID != "" {
			if _, ok := seen[ev.CampaignUUID]; !ok {
				seen[ev.CampaignUUID] = struct{}{}
				uuids = append(uuids, ev.CampaignUUID)
			}
		}
	}

	rows := make([]struct {
		ID   int    `db:"id"`
		UUID string `db:"uuid"`
	}, 0, len(uuids))
	if len(uuids) > 0 {
		if err := tx.Select(&rows, c.q.ResolveBounceCampaigns, pq.Array(uuids)); err != nil {
			return nil, c.receiptInternalError("error resolving bounce receipt campaigns", err)
		}
	}

	byUUID := make(map[string]int, len(rows))
	for _, r := range rows {
		byUUID[r.UUID] = r.ID
	}
	return byUUID, nil
}

// receiptValidationHTTPError maps a validation failure to a localized 400.
func (c *Core) receiptValidationHTTPError(e *receiptValidationError) error {
	switch e.Kind {
	case receiptErrSource:
		return echo.NewHTTPError(http.StatusBadRequest, c.i18n.T("bounces.errorSyncSource"))
	case receiptErrEventID:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncEventID", "index", strconv.Itoa(e.Index)))
	case receiptErrTarget:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncTarget", "id", e.EventID))
	case receiptErrType:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncType", "id", e.EventID, "type", e.Value))
	case receiptErrEmail:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncEmail", "id", e.EventID))
	case receiptErrUUID:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncUUID", "id", e.EventID))
	case receiptErrCampaignUUID:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncCampaignUUID", "id", e.EventID))
	case receiptErrConflict:
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("bounces.errorSyncConflict", "id", e.EventID))
	default:
		return echo.NewHTTPError(http.StatusBadRequest, c.i18n.T("globals.messages.invalidData"))
	}
}

// receiptInternalError logs the underlying error and returns a localized 500.
func (c *Core) receiptInternalError(msg string, err error) error {
	c.log.Printf("%s: %v", msg, err)
	return echo.NewHTTPError(http.StatusInternalServerError, c.i18n.T("globals.messages.internalError"))
}
