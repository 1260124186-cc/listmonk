package models

import (
	"encoding/json"
	"time"
)

const (
	BounceTypeHard      = "hard"
	BounceTypeSoft      = "soft"
	BounceTypeComplaint = "complaint"
)

// Bounce represents a single bounce event.
type Bounce struct {
	ID        int             `db:"id" json:"id"`
	Type      string          `db:"type" json:"type"`
	Source    string          `db:"source" json:"source"`
	Meta      json.RawMessage `db:"meta" json:"meta"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`

	// One of these should be provided.
	Email            string `db:"email" json:"email,omitempty"`
	SubscriberUUID   string `db:"subscriber_uuid" json:"subscriber_uuid,omitempty"`
	SubscriberID     int    `db:"subscriber_id" json:"subscriber_id,omitempty"`
	SubscriberName   string `db:"subscriber_name" json:"-"`
	SubscriberStatus string `db:"subscriber_status" json:"subscriber_status"`

	CampaignUUID string           `db:"campaign_uuid" json:"campaign_uuid,omitempty"`
	Campaign     *json.RawMessage `db:"campaign" json:"campaign"`

	// Pseudofield for getting the total number of bounces
	// in searches and queries.
	Total int `db:"total" json:"-"`
}

// BounceReceiptEvent is a single provider-identified bounce receipt inside a
// batch sync request. EventID is the provider's unique event identifier and is
// idempotent: re-submitting the same (source, event_id) never double counts.
type BounceReceiptEvent struct {
	EventID        string          `json:"event_id"`
	Email          string          `json:"email,omitempty"`
	SubscriberUUID string          `json:"subscriber_uuid,omitempty"`
	Type           string          `json:"type"`
	CampaignUUID   string          `json:"campaign_uuid,omitempty"`
	Meta           json.RawMessage `json:"meta,omitempty"`
	CreatedAt      time.Time       `json:"created_at,omitempty"`
}

// BounceReceiptBatch is a full snapshot of a provider's active bounce receipts.
// Managed events of the same source that are absent from Events are retracted.
// BatchID, when provided, makes retried submissions of the same batch a no-op.
type BounceReceiptBatch struct {
	Source  string               `json:"source"`
	BatchID string               `json:"batch_id,omitempty"`
	Events  []BounceReceiptEvent `json:"events"`
}

// BounceReceiptResult is the outcome of applying a receipt batch.
type BounceReceiptResult struct {
	Source  string `json:"source"`
	BatchID string `json:"batch_id,omitempty"`
	// Replayed is true when a previously processed batch with the same
	// (source, batch_id) was returned without re-applying anything.
	Replayed bool `json:"replayed"`
	// Received is the number of distinct events in the request after
	// in-batch de-duplication.
	Received int `json:"received"`
	// Inserted and Updated count events that were newly stored or corrected.
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	// Unchanged counts events whose stored state already matched the receipt.
	Unchanged int `json:"unchanged"`
	// Retracted counts managed events absent from the batch that were withdrawn.
	Retracted int `json:"retracted"`
	// Blocklisted, Unsubscribed and Reenabled count subscriber state changes
	// performed while reconciling bounce counts.
	Blocklisted  int `json:"blocklisted"`
	Unsubscribed int `json:"unsubscribed"`
	Reenabled    int `json:"reenabled"`
}

// BounceCampaign is parsed from the bounce's raw campaign JSON for rendering in HTML views.
type BounceCampaign struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Campaigns unmarshals the bounce's raw campaign JSON, or returns nil if the
// bounce isn't associated with a campaign.
func (b Bounce) Campaigns() *BounceCampaign {
	if b.Campaign == nil || len(*b.Campaign) == 0 {
		return nil
	}

	var out BounceCampaign
	if err := json.Unmarshal(*b.Campaign, &out); err != nil {
		return nil
	}

	return &out
}
