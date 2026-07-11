CREATE TABLE connector_instance (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    name TEXT NOT NULL,
    connector_type TEXT NOT NULL,
    capabilities JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(capabilities) = 'object'),
    config JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_by UUID NOT NULL REFERENCES "user"(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, key)
);

CREATE TABLE issue_template (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    connector_id UUID NOT NULL REFERENCES connector_instance(id) ON DELETE CASCADE,
    template_key TEXT NOT NULL,
    version INT NOT NULL CHECK (version > 0),
    name TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    priority INT NOT NULL DEFAULT 0,
    match_source_status TEXT,
    match_labels_any TEXT[] NOT NULL DEFAULT '{}',
    match_fields JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(match_fields) = 'object'),
    title_prefix TEXT NOT NULL DEFAULT '',
    description_source TEXT NOT NULL DEFAULT 'none' CHECK (description_source IN ('none', 'summary', 'title')),
    status TEXT NOT NULL DEFAULT 'todo' CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'cancelled')),
    issue_priority TEXT NOT NULL DEFAULT 'none' CHECK (issue_priority IN ('urgent', 'high', 'medium', 'low', 'none')),
    assignee_type TEXT CHECK (assignee_type IN ('member', 'agent', 'squad')),
    assignee_id UUID,
    auto_start BOOLEAN NOT NULL DEFAULT true,
    created_by UUID NOT NULL REFERENCES "user"(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((assignee_type IS NULL) = (assignee_id IS NULL)),
    UNIQUE (workspace_id, connector_id, template_key, version)
);

CREATE UNIQUE INDEX issue_template_one_enabled_version
    ON issue_template (workspace_id, connector_id, template_key) WHERE enabled;
CREATE INDEX issue_template_match
    ON issue_template (workspace_id, connector_id, enabled, priority DESC, id);

ALTER TABLE external_record
    ADD COLUMN connector_id UUID REFERENCES connector_instance(id) ON DELETE SET NULL,
    ADD COLUMN labels TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN fields JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(fields) = 'object'),
    DROP CONSTRAINT external_record_workspace_id_source_type_external_id_key;
CREATE UNIQUE INDEX external_record_identity
    ON external_record (workspace_id, connector_id, source_type, external_id) NULLS NOT DISTINCT;
CREATE INDEX external_record_connector ON external_record (workspace_id, connector_id, last_seen_at DESC);

ALTER TABLE integration_ingest_attempt
    ADD COLUMN connector_id UUID REFERENCES connector_instance(id) ON DELETE SET NULL,
    ADD COLUMN issue_template_id UUID REFERENCES issue_template(id) ON DELETE SET NULL,
    ADD COLUMN issue_template_version INT,
    ADD COLUMN request_fingerprint TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT integration_ingest_attempt_workspace_id_source_type_idempot_key;
CREATE UNIQUE INDEX integration_ingest_attempt_identity
    ON integration_ingest_attempt (workspace_id, connector_id, source_type, idempotency_key) NULLS NOT DISTINCT;
CREATE INDEX integration_ingest_attempt_template
    ON integration_ingest_attempt (workspace_id, connector_id, issue_template_id, created_at DESC);
