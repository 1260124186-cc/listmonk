-- name: record-bounce
-- Insert a bounce and count the bounces for the subscriber and either unsubscribe them,
WITH sub AS (
    SELECT id, status FROM subscribers WHERE CASE WHEN $1 != '' THEN uuid = $1::UUID ELSE email = $2 END
),
camp AS (
    SELECT id FROM campaigns WHERE $3 != '' AND uuid = $3::UUID
),
num AS (
    -- Add a +1 to include the current insertion that is happening.
    SELECT COUNT(*) + 1 AS num FROM bounces WHERE subscriber_id = (SELECT id FROM sub) AND type = $4
),
-- block1 and block2 will run when $8 = 'blocklist' and the number of bounces exceed $8.
block1 AS (
    UPDATE subscribers SET status='blocklisted'
    WHERE $9 = 'blocklist' AND (SELECT num FROM num) >= $8 AND id = (SELECT id FROM sub) AND (SELECT status FROM sub) != 'blocklisted'
),
block2 AS (
    UPDATE subscriber_lists SET status='unsubscribed'
    WHERE $9 = 'unsubscribe' AND (SELECT num FROM num) >= $8 AND subscriber_id = (SELECT id FROM sub) AND (SELECT status FROM sub) != 'blocklisted'
),
bounce AS (
    -- Record the bounce if the subscriber is not already blocklisted;
    INSERT INTO bounces (subscriber_id, campaign_id, type, source, meta, created_at)
    SELECT (SELECT id FROM sub), (SELECT id FROM camp), $4, $5, $6, $7
    WHERE NOT EXISTS (SELECT 1 WHERE (SELECT status FROM sub) = 'blocklisted' OR (SELECT num FROM num) > $8)
)
-- This delete  will only run when $9 = 'delete' and the number of bounces exceed $8.
DELETE FROM subscribers
    WHERE $9 = 'delete' AND (SELECT num FROM num) >= $8 AND id = (SELECT id FROM sub);

-- name: query-bounces
SELECT COUNT(*) OVER () AS total,
    bounces.id,
    bounces.type,
    bounces.source,
    bounces.meta,
    bounces.created_at,
    bounces.subscriber_id,
    subscribers.uuid AS subscriber_uuid,
    subscribers.email AS email,
    subscribers.name AS subscriber_name,
    subscribers.status as subscriber_status,
    (
        CASE WHEN bounces.campaign_id IS NOT NULL
        THEN JSON_BUILD_OBJECT('id', bounces.campaign_id, 'name', campaigns.name)
        ELSE NULL END
    ) AS campaign
FROM bounces
LEFT JOIN subscribers ON (subscribers.id = bounces.subscriber_id)
LEFT JOIN campaigns ON (campaigns.id = bounces.campaign_id)
WHERE ($1 = 0 OR bounces.id = $1)
    AND ($2 = 0 OR bounces.campaign_id = $2)
    AND ($3 = 0 OR bounces.subscriber_id = $3)
    AND ($4 = '' OR bounces.source = $4)
    AND ($5 = '' OR bounces.type = $5::bounce_type)
    AND ($8 OR EXISTS (
        SELECT 1 FROM subscriber_lists
        WHERE subscriber_id = bounces.subscriber_id AND list_id = ANY($9::INT[])
    ))
ORDER BY %order% OFFSET $6 LIMIT (CASE WHEN $7 < 1 THEN NULL ELSE $7 END);

-- name: delete-bounces
DELETE FROM bounces WHERE (CASE WHEN $2 THEN
    ($3 = 0 OR campaign_id = $3)
    AND ($4 = '' OR source = $4)
    AND ($5 = '' OR type = $5::bounce_type)
ELSE id = ANY($1) END)
    AND ($6 OR EXISTS (
        SELECT 1 FROM subscriber_lists
        WHERE subscriber_id = bounces.subscriber_id AND list_id = ANY($7::INT[])
    ));

-- name: delete-bounces-by-subscriber
WITH sub AS (
    SELECT id FROM subscribers WHERE CASE WHEN $1 > 0 THEN id = $1 ELSE uuid = $2 END
)
DELETE FROM bounces WHERE subscriber_id = (SELECT id FROM sub);

-- name: blocklist-bounced-subscribers
WITH subs AS (
    SELECT subscriber_id FROM bounces
    WHERE (CASE WHEN CARDINALITY($1::INT[]) > 0 THEN id = ANY($1)
    ELSE ($2 = 0 OR campaign_id = $2)
        AND ($3 = '' OR source = $3)
        AND ($4 = '' OR type = $4::bounce_type)
    END)
    AND ($5 OR EXISTS (
        SELECT 1 FROM subscriber_lists
        WHERE subscriber_id = bounces.subscriber_id AND list_id = ANY($6::INT[])
    ))
),
b AS (
    UPDATE subscribers SET status='blocklisted', updated_at=NOW()
    WHERE id = ANY(SELECT subscriber_id FROM subs)
)
UPDATE subscriber_lists SET status='unsubscribed', updated_at=NOW()
    WHERE subscriber_id = ANY(SELECT subscriber_id FROM subs);

-- name: lock-bounce-source
-- Serializes concurrent receipt batches for the same source so retries and
-- out-of-order deliveries converge inside a single transaction.
SELECT pg_advisory_xact_lock(HASHTEXT($1));

-- name: get-bounce-receipt-batch
SELECT result FROM bounce_receipt_batches WHERE source = $1 AND batch_id = $2;

-- name: insert-bounce-receipt-batch
INSERT INTO bounce_receipt_batches (source, batch_id, result) VALUES ($1, $2, $3::JSONB);

-- name: resolve-bounce-subscribers
SELECT id, LOWER(email) AS email, UUID::TEXT AS uuid FROM subscribers
    WHERE LOWER(email) = ANY($1::TEXT[]) OR UUID::TEXT = ANY($2::TEXT[]);

-- name: resolve-bounce-campaigns
SELECT id, UUID::TEXT AS uuid FROM campaigns WHERE UUID::TEXT = ANY($1::TEXT[]);

-- name: get-bounce-event
SELECT id, subscriber_id, campaign_id, type, meta, event_created_at
    FROM bounce_events WHERE source = $1 AND event_id = $2;

-- name: upsert-bounce-event
-- A NULL $7 means the provider did not send a timestamp: NOW() is used on
-- insert and the stored event_created_at is preserved on conflict so retries
-- never look like corrections.
INSERT INTO bounce_events (source, event_id, subscriber_id, campaign_id, type, meta, event_created_at)
VALUES ($1, $2, $3, $4, $5::bounce_type, $6::JSONB, COALESCE($7, NOW()))
ON CONFLICT (source, event_id) DO UPDATE SET
    subscriber_id = EXCLUDED.subscriber_id,
    campaign_id = EXCLUDED.campaign_id,
    type = EXCLUDED.type,
    meta = EXCLUDED.meta,
    event_created_at = COALESCE($7, bounce_events.event_created_at),
    updated_at = NOW()
RETURNING id;

-- name: upsert-managed-bounce
-- Mirrors a bounce_event into the bounces table 1:1. Type corrections overwrite
-- the old row; re-submission of an unchanged event is a no-op via ON CONFLICT.
INSERT INTO bounces (subscriber_id, campaign_id, type, source, meta, created_at, bounce_event_id)
VALUES ($1, $2, $3::bounce_type, $4, $5::JSONB, COALESCE($6, NOW()), $7)
ON CONFLICT (bounce_event_id) WHERE bounce_event_id IS NOT NULL DO UPDATE SET
    subscriber_id = EXCLUDED.subscriber_id,
    campaign_id = EXCLUDED.campaign_id,
    type = EXCLUDED.type,
    meta = EXCLUDED.meta,
    created_at = COALESCE($6, bounces.created_at);

-- name: get-retracting-bounce-events
-- Managed events of the source that the latest receipt no longer contains.
-- With an empty input array this matches every event of the source.
SELECT id, subscriber_id FROM bounce_events
    WHERE source = $1 AND event_id <> ALL($2::TEXT[]);

-- name: delete-bounce-events
-- The bounces rows linked through bounce_event_id are removed by ON DELETE CASCADE.
DELETE FROM bounce_events WHERE source = $1 AND id = ANY($2::INT[]);

-- name: get-subscriber-for-bounce-reconcile
SELECT id, status, updated_at FROM subscribers WHERE id = $1 FOR UPDATE;

-- name: get-bounce-counts
SELECT type, COUNT(*) AS num FROM bounces WHERE subscriber_id = $1 GROUP BY type;

-- name: get-bounce-status-ledger
SELECT subscriber_id, prev_sub_status, prev_list_statuses, managed_blocklist,
    managed_unsubscribe, updated_at
    FROM bounce_status_ledger WHERE subscriber_id = $1 FOR UPDATE;

-- name: upsert-bounce-status-ledger
-- On conflict the original snapshot (prev_* columns) is intentionally preserved;
-- only the ownership flags and timestamp are refreshed.
INSERT INTO bounce_status_ledger (subscriber_id, prev_sub_status, prev_list_statuses,
    managed_blocklist, managed_unsubscribe, updated_at)
VALUES ($1, $2::subscriber_status, $3::JSONB, $4, $5, NOW())
ON CONFLICT (subscriber_id) DO UPDATE SET
    managed_blocklist = EXCLUDED.managed_blocklist,
    managed_unsubscribe = EXCLUDED.managed_unsubscribe,
    updated_at = NOW();

-- name: delete-bounce-status-ledger
DELETE FROM bounce_status_ledger WHERE subscriber_id = $1;

-- name: snapshot-bounce-list-statuses
-- Snapshot of the per-list subscription statuses before reconciliation touches
-- them, so retraction can restore only rows the reconciliation last wrote.
SELECT COALESCE(JSONB_OBJECT_AGG(list_id, status), '{}'::JSONB) AS statuses
    FROM subscriber_lists WHERE subscriber_id = $1;

-- name: apply-bounce-blocklist-subscriber
UPDATE subscribers SET status = 'blocklisted', updated_at = NOW()
    WHERE id = $1 AND status <> 'blocklisted';

-- name: apply-bounce-blocklist-lists
UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
    WHERE subscriber_id = $1;

-- name: apply-bounce-unsubscribe-lists
UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
    WHERE subscriber_id = $1 AND status <> 'unsubscribed';

-- name: restore-bounce-subscriber-status
-- Revert the blocklist only when the subscriber row has not been edited since
-- the reconciliation applied it. A newer updated_at means a manual (or other)
-- intervention owns the current blocklisted state and it must not be touched.
UPDATE subscribers SET status = $2::subscriber_status, updated_at = NOW()
    WHERE id = $1 AND status = 'blocklisted' AND updated_at <= $3;

-- name: restore-bounce-list-statuses
-- Restore only list rows that reconciliation last wrote (updated_at has not
-- moved since), leaving later manual unsubscribes untouched.
UPDATE subscriber_lists sl
SET status = j.status::subscription_status, updated_at = NOW()
FROM JSONB_EACH_TEXT($2::JSONB) AS j(list_id, status)
WHERE sl.subscriber_id = $1
    AND sl.list_id = j.list_id::INT
    AND sl.status = 'unsubscribed'
    AND sl.updated_at <= $3;

