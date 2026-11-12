package core

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
)

func validEvent(id, target string) models.BounceReceiptEvent {
	ev := models.BounceReceiptEvent{
		EventID: id,
		Type:    models.BounceTypeHard,
		Meta:    json.RawMessage(`{"k":"v"}`),
	}
	if len(target) == 36 {
		ev.SubscriberUUID = target
	} else {
		ev.Email = target
	}
	return ev
}

const testUUID = "32ca1f3e-1a1d-42e1-af04-df0757f420f3"

func TestValidateBounceReceiptBatch(t *testing.T) {
	tests := []struct {
		name      string
		batch     models.BounceReceiptBatch
		wantErr   receiptValidationErrorKind
		wantEvent string
		wantCount int
	}{
		{
			name:    "missing source",
			batch:   models.BounceReceiptBatch{Events: []models.BounceReceiptEvent{validEvent("e1", "a@example.com")}},
			wantErr: receiptErrSource,
		},
		{
			name: "missing event id",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{Email: "a@example.com", Type: models.BounceTypeHard},
				},
			},
			wantErr: receiptErrEventID,
		},
		{
			name: "invalid type",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{EventID: "e1", Email: "a@example.com", Type: "bounce"},
				},
			},
			wantErr:   receiptErrType,
			wantEvent: "e1",
		},
		{
			name: "missing target",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{EventID: "e1", Type: models.BounceTypeHard},
				},
			},
			wantErr:   receiptErrTarget,
			wantEvent: "e1",
		},
		{
			name: "invalid email",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{EventID: "e1", Email: "not-an-email", Type: models.BounceTypeHard},
				},
			},
			wantErr:   receiptErrEmail,
			wantEvent: "e1",
		},
		{
			name: "invalid subscriber uuid",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{EventID: "e1", SubscriberUUID: "abcd", Type: models.BounceTypeHard},
				},
			},
			wantErr:   receiptErrUUID,
			wantEvent: "e1",
		},
		{
			name: "invalid campaign uuid",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					{EventID: "e1", Email: "a@example.com", Type: models.BounceTypeHard, CampaignUUID: "nope"},
				},
			},
			wantErr:   receiptErrCampaignUUID,
			wantEvent: "e1",
		},
		{
			name: "conflicting duplicate event id",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					validEvent("e1", "a@example.com"),
					{EventID: "e1", Email: "b@example.com", Type: models.BounceTypeHard},
				},
			},
			wantErr:   receiptErrConflict,
			wantEvent: "e1",
		},
		{
			name: "identical duplicate is de-duplicated",
			batch: models.BounceReceiptBatch{
				Source: "ses",
				Events: []models.BounceReceiptEvent{
					validEvent("e1", "a@example.com"),
					validEvent("e1", "a@example.com"),
					validEvent("e2", testUUID),
				},
			},
			wantCount: 2,
		},
		{
			name: "valid batch is normalized",
			batch: models.BounceReceiptBatch{
				Source: "  ses  ",
				Events: []models.BounceReceiptEvent{
					{EventID: " e1 ", Email: " A@Example.COM ", Type: models.BounceTypeSoft},
				},
			},
			wantCount: 1,
		},
		{
			name:      "empty snapshot is valid",
			batch:     models.BounceReceiptBatch{Source: "ses"},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, events, verr := validateBounceReceiptBatch(tt.batch)
			if tt.wantErr != "" {
				if verr == nil {
					t.Fatalf("expected error %s, got nil", tt.wantErr)
				}
				if verr.Kind != tt.wantErr {
					t.Fatalf("expected error kind %s, got %s", tt.wantErr, verr.Kind)
				}
				if tt.wantEvent != "" && verr.EventID != tt.wantEvent {
					t.Fatalf("expected event id %s, got %s", tt.wantEvent, verr.EventID)
				}
				return
			}
			if verr != nil {
				t.Fatalf("unexpected error: %v", verr)
			}
			if source != "ses" {
				t.Fatalf("expected trimmed source 'ses', got %q", source)
			}
			if len(events) != tt.wantCount {
				t.Fatalf("expected %d events, got %d", tt.wantCount, len(events))
			}
			for _, ev := range events {
				// An omitted provider timestamp must be preserved as zero so
				// retries do not look like corrections.
				if !ev.CreatedAt.IsZero() {
					t.Fatalf("event %s created_at should stay zero when omitted", ev.EventID)
				}
				if len(ev.Meta) == 0 {
					t.Fatalf("event %s meta not defaulted", ev.EventID)
				}
			}
		})
	}
}

func TestValidateBounceReceiptBatchNormalizesEmail(t *testing.T) {
	batch := models.BounceReceiptBatch{
		Source: "ses",
		Events: []models.BounceReceiptEvent{
			{EventID: "e1", Email: " A@Example.COM ", Type: models.BounceTypeHard},
		},
	}
	_, events, verr := validateBounceReceiptBatch(batch)
	if verr != nil {
		t.Fatalf("unexpected error: %v", verr)
	}
	if events[0].Email != "a@example.com" {
		t.Fatalf("expected sanitized email, got %q", events[0].Email)
	}
}

func TestDesiredBounceAction(t *testing.T) {
	actions := map[string]struct {
		Count  int
		Action string
	}{
		models.BounceTypeSoft:      {Count: 2, Action: bounceActionNone},
		models.BounceTypeHard:      {Count: 1, Action: bounceActionBlocklist},
		models.BounceTypeComplaint: {Count: 1, Action: bounceActionUnsubscribe},
	}

	if got := desiredBounceAction(map[string]int{models.BounceTypeSoft: 1}, actions); got != bounceActionNone {
		t.Fatalf("expected none below threshold, got %s", got)
	}
	if got := desiredBounceAction(map[string]int{models.BounceTypeSoft: 2}, actions); got != bounceActionNone {
		t.Fatalf("expected none, got %s", got)
	}
	if got := desiredBounceAction(map[string]int{models.BounceTypeComplaint: 1}, actions); got != bounceActionUnsubscribe {
		t.Fatalf("expected unsubscribe, got %s", got)
	}
	if got := desiredBounceAction(map[string]int{
		models.BounceTypeSoft: 2,
		models.BounceTypeHard: 1,
	}, actions); got != bounceActionBlocklist {
		t.Fatalf("expected blocklist to win over weaker actions, got %s", got)
	}

	// The irreversible delete action is coerced to blocklist.
	deleteActions := map[string]struct {
		Count  int
		Action string
	}{
		models.BounceTypeHard: {Count: 1, Action: bounceActionDelete},
	}
	if got := desiredBounceAction(map[string]int{models.BounceTypeHard: 1}, deleteActions); got != bounceActionBlocklist {
		t.Fatalf("expected delete to be coerced to blocklist, got %s", got)
	}
}

func TestEventEqualsReceipt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := bounceEventRow{
		ID:             1,
		SubscriberID:   10,
		CampaignID:     sql.NullInt64{Int64: 3, Valid: true},
		Type:           models.BounceTypeHard,
		Meta:           json.RawMessage(`{"k":"v"}`),
		EventCreatedAt: now,
	}
	ev := models.BounceReceiptEvent{
		EventID:   "e1",
		Email:     "a@example.com",
		Type:      models.BounceTypeHard,
		Meta:      json.RawMessage(`{ "k": "v" }`),
		CreatedAt: now,
	}
	camp := sql.NullInt64{Int64: 3, Valid: true}
	created := sql.NullTime{Time: now, Valid: true}

	if !eventEqualsReceipt(row, 10, camp, created, ev) {
		t.Fatalf("expected equivalent events to match despite meta whitespace")
	}

	// An omitted timestamp (NULL) never counts as a change on retry.
	if !eventEqualsReceipt(row, 10, camp, sql.NullTime{}, ev) {
		t.Fatalf("expected NULL incoming timestamp to be ignored")
	}

	// An explicit timestamp change must register as a change.
	later := sql.NullTime{Time: now.Add(time.Second), Valid: true}
	if eventEqualsReceipt(row, 10, camp, later, ev) {
		t.Fatalf("expected timestamp change to be detected as a change")
	}

	// Type correction must register as a change.
	ev.Type = models.BounceTypeSoft
	if eventEqualsReceipt(row, 10, camp, created, ev) {
		t.Fatalf("expected type correction to be detected as a change")
	}

	// Subscriber correction must register as a change.
	ev.Type = models.BounceTypeHard
	if eventEqualsReceipt(row, 11, camp, created, ev) {
		t.Fatalf("expected subscriber correction to be detected as a change")
	}

	// Campaign removal must register as a change.
	if eventEqualsReceipt(row, 10, sql.NullInt64{}, created, ev) {
		t.Fatalf("expected campaign removal to be detected as a change")
	}
}
