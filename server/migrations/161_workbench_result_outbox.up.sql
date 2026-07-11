ALTER TABLE external_record
    ADD CONSTRAINT external_record_workspace_id_id_connector_id_key UNIQUE (workspace_id, id, connector_id);
ALTER TABLE issue
    ADD CONSTRAINT issue_workspace_id_id_key UNIQUE (workspace_id, id);
ALTER TABLE agent_task_queue
    ADD CONSTRAINT agent_task_queue_id_issue_id_key UNIQUE (id, issue_id);

CREATE TABLE workbench_result_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    connector_id UUID NOT NULL,
    external_record_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    task_id UUID NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('completed', 'failed')),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'leased', 'delivered', 'terminal_failed')),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    idempotency_key TEXT NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    last_status INT,
    last_error TEXT,
    delivered_at TIMESTAMPTZ,
    terminal_failed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE NO ACTION,
    FOREIGN KEY (workspace_id, external_record_id, connector_id)
        REFERENCES external_record(workspace_id, id, connector_id) ON DELETE NO ACTION,
    FOREIGN KEY (workspace_id, issue_id)
        REFERENCES issue(workspace_id, id) ON DELETE NO ACTION,
    FOREIGN KEY (task_id, issue_id)
        REFERENCES agent_task_queue(id, issue_id) ON DELETE NO ACTION,
    UNIQUE (task_id, outcome),
    CHECK (idempotency_key = 'workbench-result:v1:' || task_id::text || ':' || outcome),
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL)),
    CHECK ((status = 'terminal_failed') = (terminal_failed_at IS NOT NULL)),
    CHECK (delivered_at IS NULL OR terminal_failed_at IS NULL)
);

CREATE INDEX workbench_result_outbox_workspace_connector
    ON workbench_result_outbox (workspace_id, connector_id);
CREATE INDEX workbench_result_outbox_workspace_external_record
    ON workbench_result_outbox (workspace_id, external_record_id);
CREATE INDEX workbench_result_outbox_workspace_issue
    ON workbench_result_outbox (workspace_id, issue_id);

CREATE INDEX workbench_result_outbox_ready
    ON workbench_result_outbox (next_attempt_at, created_at, id)
    WHERE status = 'pending';
CREATE INDEX workbench_result_outbox_expired_lease
    ON workbench_result_outbox (lease_expires_at, created_at, id)
    WHERE status = 'leased';
