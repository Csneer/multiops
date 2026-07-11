CREATE TABLE external_record (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    source_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    external_key TEXT,
    title TEXT NOT NULL,
    summary TEXT,
    source_status TEXT,
    source_url TEXT,
    schema_version TEXT NOT NULL DEFAULT 'v1',
    raw_payload_ref TEXT,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, source_type, external_id)
);

CREATE TABLE issue_external_record_binding (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    issue_id UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    external_record_id UUID NOT NULL REFERENCES external_record(id) ON DELETE CASCADE,
    binding_role TEXT NOT NULL DEFAULT 'primary' CHECK (binding_role IN ('primary', 'related')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issue_id, external_record_id)
);

CREATE TABLE integration_ingest_attempt (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    source_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    external_record_id UUID REFERENCES external_record(id) ON DELETE SET NULL,
    issue_id UUID REFERENCES issue(id) ON DELETE SET NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('processing', 'created', 'updated', 'duplicate', 'failed')),
    error_summary TEXT,
    attempt_count INT NOT NULL DEFAULT 1 CHECK (attempt_count > 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, source_type, idempotency_key)
);

CREATE INDEX idx_external_record_workspace_last_seen
    ON external_record (workspace_id, last_seen_at DESC);
CREATE INDEX idx_issue_external_record_binding_issue
    ON issue_external_record_binding (workspace_id, issue_id, created_at DESC);
CREATE INDEX idx_integration_ingest_attempt_record
    ON integration_ingest_attempt (workspace_id, external_record_id, created_at DESC);
CREATE INDEX idx_integration_ingest_attempt_issue
    ON integration_ingest_attempt (workspace_id, issue_id, created_at DESC);
