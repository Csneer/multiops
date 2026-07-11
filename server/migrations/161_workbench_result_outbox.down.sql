DROP TABLE IF EXISTS workbench_result_outbox;

ALTER TABLE agent_task_queue
    DROP CONSTRAINT IF EXISTS agent_task_queue_id_issue_id_key;
ALTER TABLE issue
    DROP CONSTRAINT IF EXISTS issue_workspace_id_id_key;
ALTER TABLE external_record
    DROP CONSTRAINT IF EXISTS external_record_workspace_id_id_connector_id_key;
