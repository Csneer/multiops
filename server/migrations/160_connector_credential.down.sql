DROP TABLE connector_credential;

ALTER TABLE integration_ingest_attempt
    DROP CONSTRAINT integration_ingest_attempt_workspace_connector_fkey,
    ADD CONSTRAINT integration_ingest_attempt_connector_id_fkey
        FOREIGN KEY (connector_id) REFERENCES connector_instance(id) ON DELETE SET NULL;

ALTER TABLE external_record
    DROP CONSTRAINT external_record_workspace_connector_fkey,
    ADD CONSTRAINT external_record_connector_id_fkey
        FOREIGN KEY (connector_id) REFERENCES connector_instance(id) ON DELETE SET NULL;

ALTER TABLE issue_template
    DROP CONSTRAINT issue_template_workspace_connector_fkey,
    ADD CONSTRAINT issue_template_connector_id_fkey
        FOREIGN KEY (connector_id) REFERENCES connector_instance(id) ON DELETE CASCADE;

ALTER TABLE connector_instance
    DROP CONSTRAINT connector_instance_workspace_id_id_key;
