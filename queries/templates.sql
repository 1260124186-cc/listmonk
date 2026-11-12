-- templates
-- name: get-templates
-- Only if the second param ($2 - noBody) is true, body and body_source is returned.
SELECT id, name, type, subject,
    (CASE WHEN $2 = false THEN body ELSE '' END) as body,
    (CASE WHEN $2 = false THEN body_source ELSE NULL END) as body_source,
    is_default, template_key, managed, created_at, updated_at
    FROM templates WHERE ($1 = 0 OR id = $1) AND ($3 = '' OR type = $3::template_type)
    AND ($4 = '' OR TO_TSVECTOR(name) @@ PLAINTO_TSQUERY($4) OR name ILIKE ('%' || $4 || '%'))
    ORDER BY %order%;

-- name: get-managed-templates
-- All templates owned by a template bundle, keyed by template_key in the app.
SELECT id, name, type, subject, body, body_source, is_default, template_key, managed,
    created_at, updated_at
    FROM templates WHERE template_key IS NOT NULL ORDER BY id;

-- name: get-template-by-key
SELECT id, name, type, subject, body, body_source, is_default, template_key, managed,
    created_at, updated_at
    FROM templates WHERE template_key = $1;

-- name: insert-managed-template
INSERT INTO templates (name, type, subject, body, body_source, template_key, managed)
    VALUES ($1, $2, $3, $4, $5, $6, true) RETURNING id;

-- name: update-managed-template
-- Full overwrite of a bundle-managed row so the bundle remains the source of truth.
UPDATE templates SET
    name=$2,
    type=$3,
    subject=$4,
    body=$5,
    body_source=$6,
    updated_at=NOW()
    WHERE id=$1 AND template_key IS NOT NULL;

-- name: set-default-template-sync
-- Make $1 the sole default template. Only campaign templates can hold the flag.
WITH u AS (
    UPDATE templates SET is_default=true, updated_at=NOW()
        WHERE id=$1 AND type='campaign' RETURNING id
)
UPDATE templates SET is_default=false, updated_at=NOW() WHERE id != $1;

-- name: get-default-template-id
SELECT id FROM templates
    WHERE is_default = true AND (type='campaign' OR type='campaign_visual') LIMIT 1;

-- name: reassign-campaign-template
-- Move campaigns off a template that is about to be removed by a bundle sync.
UPDATE campaigns SET template_id=$2, updated_at=NOW() WHERE template_id=$1;

-- name: delete-managed-template
-- A default template can never be deleted here; the sync must hand over the
-- default flag to another template before removing a managed default.
DELETE FROM templates WHERE id=$1 AND is_default=false AND managed=true RETURNING id;

-- name: create-template
INSERT INTO templates (name, type, subject, body, body_source) VALUES($1, $2, $3, $4, $5) RETURNING id;

-- name: update-template
UPDATE templates SET
    name=(CASE WHEN $2 != '' THEN $2 ELSE name END),
    subject=(CASE WHEN $3 != '' THEN $3 ELSE name END),
    body=(CASE WHEN $4 != '' THEN $4 ELSE body END),
    body_source=(CASE WHEN $5 != '' THEN $5 ELSE body_source END),
    updated_at=NOW()
WHERE id = $1;

-- name: set-default-template
WITH u AS (
    UPDATE templates SET is_default=true WHERE id=$1 AND type='campaign' RETURNING id
)
UPDATE templates SET is_default=false WHERE id != $1;

-- name: delete-template
-- Delete a template as long as there's more than one. On deletion, set all campaigns
-- with that template to the default template instead.
WITH tpl AS (
    DELETE FROM templates WHERE id = $1 AND (SELECT COUNT(id) FROM templates) > 1 AND is_default = false RETURNING id
),
def AS (
    SELECT id FROM templates WHERE is_default = true AND (type='campaign' OR type='campaign_visual') LIMIT 1
),
up AS (
    UPDATE campaigns SET template_id = (SELECT id FROM def) WHERE (SELECT id FROM tpl) > 0 AND template_id = $1
)
SELECT id FROM tpl;

