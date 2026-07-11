-- =====================
-- Workbench Phase 1
-- =====================

-- name: RecordOrBumpIntegrationIngestAttempt :one
-- Atomically claims an idempotency key or records another receipt of an
-- existing key. The original outcome and links remain authoritative; callers
-- use inserted=false to return a deterministic duplicate result without
-- repeating the domain mutation.
INSERT INTO integration_ingest_attempt (
    workspace_id, source_type, idempotency_key, outcome, observed_at
) VALUES (
    $1, $2, $3, 'processing', sqlc.narg('observed_at')
)
ON CONFLICT (workspace_id, source_type, idempotency_key) DO UPDATE
SET attempt_count = integration_ingest_attempt.attempt_count + 1,
    last_attempt_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: GetIntegrationIngestAttempt :one
SELECT * FROM integration_ingest_attempt
WHERE workspace_id = $1
  AND source_type = $2
  AND idempotency_key = $3;

-- name: CompleteIntegrationIngestAttempt :one
UPDATE integration_ingest_attempt
SET external_record_id = $2,
    issue_id = sqlc.narg('issue_id'),
    outcome = $3,
    error_summary = sqlc.narg('error_summary'),
    last_attempt_at = now()
WHERE id = $1
RETURNING *;

-- name: GetExternalRecordInWorkspace :one
SELECT * FROM external_record
WHERE id = $1
  AND workspace_id = $2;

-- name: UpsertExternalRecord :one
INSERT INTO external_record (
    workspace_id, source_type, external_id, external_key, title, summary,
    source_status, source_url, schema_version, raw_payload_ref, last_seen_at
) VALUES (
    $1, $2, $3, sqlc.narg('external_key'), $4, sqlc.narg('summary'),
    sqlc.narg('source_status'), sqlc.narg('source_url'), $5,
    sqlc.narg('raw_payload_ref'), sqlc.narg('last_seen_at')
)
ON CONFLICT (workspace_id, source_type, external_id) DO UPDATE
SET external_key = EXCLUDED.external_key,
    title = EXCLUDED.title,
    summary = EXCLUDED.summary,
    source_status = EXCLUDED.source_status,
    source_url = EXCLUDED.source_url,
    schema_version = EXCLUDED.schema_version,
    raw_payload_ref = EXCLUDED.raw_payload_ref,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: CreateIssueExternalRecordBinding :one
-- The joins are defense in depth: the binding is created only when both
-- referenced records belong to the requested workspace.
INSERT INTO issue_external_record_binding (
    workspace_id, issue_id, external_record_id, binding_role
)
SELECT sqlc.arg('workspace_id'), i.id, r.id, sqlc.arg('binding_role')
FROM issue i
JOIN external_record r
  ON r.id = sqlc.arg('external_record_id')
 AND r.workspace_id = sqlc.arg('workspace_id')
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
ON CONFLICT (issue_id, external_record_id) DO NOTHING
RETURNING *;

-- name: ListIssueExternalRecordBindings :many
SELECT
    b.id AS binding_id,
    b.binding_role,
    b.created_at AS bound_at,
    r.id AS external_record_id,
    r.source_type,
    r.external_id,
    r.external_key,
    r.title,
    r.summary,
    r.source_status,
    r.source_url,
    r.schema_version,
    r.last_seen_at
FROM issue_external_record_binding b
JOIN external_record r ON r.id = b.external_record_id
WHERE b.workspace_id = $1
  AND b.issue_id = $2
ORDER BY b.created_at DESC;
