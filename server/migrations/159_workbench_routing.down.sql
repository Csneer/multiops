DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM integration_ingest_attempt
        GROUP BY workspace_id, source_type, idempotency_key
        HAVING count(*) > 1
    ) OR EXISTS (
        SELECT 1
        FROM external_record
        GROUP BY workspace_id, source_type, external_id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot roll back workbench routing while connector-isolated identities overlap';
    END IF;
END $$;

DROP INDEX IF EXISTS integration_ingest_attempt_template;
DROP INDEX IF EXISTS integration_ingest_attempt_identity;
ALTER TABLE integration_ingest_attempt
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS issue_template_version,
    DROP COLUMN IF EXISTS issue_template_id,
    DROP COLUMN IF EXISTS connector_id,
    ADD CONSTRAINT integration_ingest_attempt_workspace_id_source_type_idempot_key
        UNIQUE (workspace_id, source_type, idempotency_key);

DROP INDEX IF EXISTS external_record_connector;
DROP INDEX IF EXISTS external_record_identity;
ALTER TABLE external_record
    DROP COLUMN IF EXISTS fields,
    DROP COLUMN IF EXISTS labels,
    DROP COLUMN IF EXISTS connector_id,
    ADD CONSTRAINT external_record_workspace_id_source_type_external_id_key
        UNIQUE (workspace_id, source_type, external_id);

DROP TABLE IF EXISTS issue_template;
DROP TABLE IF EXISTS connector_instance;
