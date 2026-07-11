ALTER TABLE connector_instance
    ADD CONSTRAINT connector_instance_workspace_id_id_key UNIQUE (workspace_id, id);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM issue_template child
        LEFT JOIN connector_instance parent
          ON parent.workspace_id = child.workspace_id
         AND parent.id = child.connector_id
        WHERE parent.id IS NULL
    ) OR EXISTS (
        SELECT 1
        FROM external_record child
        LEFT JOIN connector_instance parent
          ON parent.workspace_id = child.workspace_id
         AND parent.id = child.connector_id
        WHERE child.connector_id IS NOT NULL AND parent.id IS NULL
    ) OR EXISTS (
        SELECT 1
        FROM integration_ingest_attempt child
        LEFT JOIN connector_instance parent
          ON parent.workspace_id = child.workspace_id
         AND parent.id = child.connector_id
        WHERE child.connector_id IS NOT NULL AND parent.id IS NULL
    ) THEN
        RAISE EXCEPTION 'cannot add workspace-scoped connector foreign keys: mismatched connector workspace data exists';
    END IF;
END $$;

ALTER TABLE issue_template
    DROP CONSTRAINT issue_template_connector_id_fkey,
    ADD CONSTRAINT issue_template_workspace_connector_fkey
        FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE CASCADE NOT VALID;
ALTER TABLE issue_template VALIDATE CONSTRAINT issue_template_workspace_connector_fkey;

ALTER TABLE external_record
    DROP CONSTRAINT external_record_connector_id_fkey,
    ADD CONSTRAINT external_record_workspace_connector_fkey
        FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE SET NULL (connector_id) NOT VALID;
ALTER TABLE external_record VALIDATE CONSTRAINT external_record_workspace_connector_fkey;

ALTER TABLE integration_ingest_attempt
    DROP CONSTRAINT integration_ingest_attempt_connector_id_fkey,
    ADD CONSTRAINT integration_ingest_attempt_workspace_connector_fkey
        FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE SET NULL (connector_id) NOT VALID;
ALTER TABLE integration_ingest_attempt VALIDATE CONSTRAINT integration_ingest_attempt_workspace_connector_fkey;

CREATE TABLE connector_credential (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    connector_id UUID NOT NULL,
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    token_prefix TEXT NOT NULL CHECK (token_prefix ~ '^mci_[0-9a-f]{8}$'),
    revoked_at TIMESTAMPTZ,
    created_by UUID NOT NULL REFERENCES "user"(id),
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    CHECK (last_used_at IS NULL OR last_used_at >= created_at),
    FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX connector_credential_workspace_connector
    ON connector_credential (workspace_id, connector_id, created_at DESC, id ASC);
