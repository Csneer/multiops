-- =====================
-- Workbench
-- =====================

-- name: CreateConnectorInstance :one
INSERT INTO connector_instance (
    workspace_id, key, name, connector_type, capabilities, config, enabled, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetEnabledConnectorInWorkspace :one
SELECT id, workspace_id, key, name, connector_type, capabilities, enabled, created_at, updated_at
FROM connector_instance
WHERE id = $1 AND workspace_id = $2 AND enabled;

-- name: GetConnectorInWorkspace :one
SELECT id, workspace_id, key, name, connector_type, capabilities, enabled, created_at, updated_at
FROM connector_instance
WHERE id = $1 AND workspace_id = $2;

-- name: ListConnectorsInWorkspace :many
SELECT id, workspace_id, key, name, connector_type, capabilities, enabled, created_at, updated_at
FROM connector_instance
WHERE workspace_id = $1
ORDER BY key ASC, id ASC;

-- name: DisableConnectorInWorkspace :one
UPDATE connector_instance
SET enabled = false, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING id, workspace_id, key, name, connector_type, capabilities, enabled, created_at, updated_at;

-- name: CreateConnectorCredential :one
INSERT INTO connector_credential (
    connector_id, workspace_id, name, token_hash, token_prefix, created_by
)
SELECT ci.id, ci.workspace_id, sqlc.arg('name'), sqlc.arg('token_hash'),
       sqlc.arg('token_prefix'), sqlc.arg('created_by')
FROM connector_instance ci
WHERE ci.id = sqlc.arg('connector_id')
  AND ci.workspace_id = sqlc.arg('workspace_id')
RETURNING id, connector_id, workspace_id, name, token_prefix, revoked_at,
          created_by, last_used_at, created_at;

-- name: GetActiveConnectorCredentialByHash :one
SELECT cc.id, cc.connector_id, cc.workspace_id, ci.connector_type
FROM connector_credential cc
JOIN connector_instance ci
  ON ci.id = cc.connector_id
 AND ci.workspace_id = cc.workspace_id
WHERE cc.token_hash = $1
  AND cc.revoked_at IS NULL
  AND ci.enabled;

-- name: ListConnectorCredentials :many
SELECT id, connector_id, workspace_id, name, token_prefix, revoked_at,
       created_by, last_used_at, created_at
FROM connector_credential
WHERE workspace_id = $1 AND connector_id = $2
ORDER BY created_at DESC, id ASC;

-- name: GetActiveConnectorCredentialForUpdate :one
SELECT id, connector_id, workspace_id, name, token_prefix, revoked_at,
       created_by, last_used_at, created_at
FROM connector_credential
WHERE id = $1 AND connector_id = $2 AND workspace_id = $3 AND revoked_at IS NULL
FOR UPDATE;

-- name: RevokeConnectorCredential :execrows
UPDATE connector_credential
SET revoked_at = now()
WHERE id = $1 AND connector_id = $2 AND workspace_id = $3 AND revoked_at IS NULL;

-- name: UpdateConnectorCredentialLastUsed :exec
UPDATE connector_credential
SET last_used_at = now()
WHERE id = $1 AND connector_id = $2 AND workspace_id = $3 AND revoked_at IS NULL;

-- name: LockEnabledConnectorForRouting :one
SELECT * FROM connector_instance
WHERE id = $1 AND workspace_id = $2 AND enabled
FOR UPDATE;

-- name: LockEnabledConnectorForPreview :one
SELECT id, workspace_id, key, name, connector_type, capabilities, enabled, created_at, updated_at
FROM connector_instance
WHERE id = $1 AND workspace_id = $2 AND enabled
FOR SHARE;

-- name: LockConnectorForCredentialManagement :one
SELECT id FROM connector_instance
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: LockConnectorTemplateKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(
    sqlc.arg('workspace_id')::text || ':' || sqlc.arg('connector_id')::text || ':' || sqlc.arg('template_key'), 0
));

-- name: DisableEnabledIssueTemplate :exec
UPDATE issue_template SET enabled = false
WHERE workspace_id = $1 AND connector_id = $2 AND template_key = $3 AND enabled;

-- name: GetNextIssueTemplateVersion :one
SELECT COALESCE(MAX(version), 0)::int + 1 AS version
FROM issue_template
WHERE workspace_id = $1 AND connector_id = $2 AND template_key = $3;

-- name: CreateIssueTemplate :one
INSERT INTO issue_template (
    workspace_id, connector_id, template_key, version, name, enabled, priority,
    match_source_status, match_labels_any, match_fields, title_prefix,
    description_source, status, issue_priority, assignee_type, assignee_id,
    auto_start, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, sqlc.narg('match_source_status'), $8, $9,
    $10, $11, $12, $13, sqlc.narg('assignee_type'), sqlc.narg('assignee_id'), $14, $15
)
RETURNING *;

-- name: ListIssueTemplateHistory :many
SELECT * FROM issue_template
WHERE workspace_id = $1 AND connector_id = $2
ORDER BY template_key ASC, version DESC, id ASC;

-- name: ListActiveIssueTemplates :many
SELECT * FROM issue_template
WHERE workspace_id = $1 AND connector_id = $2 AND enabled
ORDER BY template_key ASC, id ASC;

-- name: SelectMatchingIssueTemplate :one
SELECT * FROM issue_template
WHERE workspace_id = sqlc.arg('workspace_id')
  AND connector_id = sqlc.arg('connector_id')
  AND enabled
  AND (match_source_status IS NULL OR match_source_status = sqlc.narg('source_status'))
  AND (cardinality(match_labels_any) = 0 OR match_labels_any && sqlc.arg('labels')::text[])
  AND match_fields <@ sqlc.arg('fields')::jsonb
ORDER BY priority DESC, id ASC
LIMIT 1;

-- name: RecordOrBumpIntegrationIngestAttempt :one
-- Atomically claims an idempotency key or records another receipt of an
-- existing key. The original outcome and links remain authoritative; callers
-- use inserted=false to return a deterministic duplicate result without
-- repeating the domain mutation.
INSERT INTO integration_ingest_attempt (
    workspace_id, source_type, idempotency_key, connector_id, request_fingerprint,
    outcome, observed_at
) VALUES (
    $1, $2, $3, sqlc.narg('connector_id'), $4, 'processing', sqlc.narg('observed_at')
)
ON CONFLICT (workspace_id, connector_id, source_type, idempotency_key) DO UPDATE
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
    connector_id = sqlc.narg('connector_id'),
    issue_template_id = sqlc.narg('issue_template_id'),
    issue_template_version = sqlc.narg('issue_template_version'),
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
    workspace_id, source_type, external_id, connector_id, external_key, title, summary,
    source_status, source_url, schema_version, raw_payload_ref, labels, fields, last_seen_at
) VALUES (
    $1, $2, $3, sqlc.narg('connector_id'), sqlc.narg('external_key'), $4, sqlc.narg('summary'),
    sqlc.narg('source_status'), sqlc.narg('source_url'), $5,
    sqlc.narg('raw_payload_ref'), $6, $7, sqlc.narg('last_seen_at')
)
ON CONFLICT (workspace_id, connector_id, source_type, external_id) DO UPDATE
SET connector_id = EXCLUDED.connector_id,
    external_key = EXCLUDED.external_key,
    title = EXCLUDED.title,
    summary = EXCLUDED.summary,
    source_status = EXCLUDED.source_status,
    source_url = EXCLUDED.source_url,
    schema_version = EXCLUDED.schema_version,
    raw_payload_ref = EXCLUDED.raw_payload_ref,
    labels = EXCLUDED.labels,
    fields = EXCLUDED.fields,
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

-- name: LockIssueForNeverEnqueuedReconciliation :one
-- The issue row lock serializes compensation for one issue. Callers check task
-- history only after acquiring this lock, in the same transaction.
SELECT i.*
FROM issue i
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
FOR UPDATE;

-- name: HasAnyTaskHistoryForIssue :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue t WHERE t.issue_id = sqlc.arg('issue_id')
);

-- name: ListIssueExternalRecordBindings :many
SELECT
    b.id AS binding_id,
    b.binding_role,
    b.created_at AS bound_at,
    r.id AS external_record_id,
    r.connector_id,
    r.source_type,
    r.external_id,
    r.external_key,
    r.title,
    r.summary,
    r.source_status,
    r.source_url,
    r.schema_version,
    r.labels,
    r.fields,
    r.last_seen_at
FROM issue_external_record_binding b
JOIN external_record r ON r.id = b.external_record_id
WHERE b.workspace_id = $1
  AND b.issue_id = $2
ORDER BY b.created_at DESC;
