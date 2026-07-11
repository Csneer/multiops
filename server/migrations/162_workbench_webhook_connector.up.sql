CREATE TABLE workbench_webhook_connector (
    connector_id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    config_version INT NOT NULL CHECK (config_version = 1),
    endpoint_url TEXT NOT NULL,
    timeout_ms INT NOT NULL CHECK (timeout_ms BETWEEN 1000 AND 30000),
    signing_secret_encrypted BYTEA NOT NULL CHECK (octet_length(signing_secret_encrypted) >= 29),
    signing_secret_prefix TEXT NOT NULL CHECK (signing_secret_prefix ~ '^mws_[0-9a-f]{8}$'),
    created_by UUID NOT NULL REFERENCES "user"(id),
    updated_by UUID NOT NULL REFERENCES "user"(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, connector_id)
        REFERENCES connector_instance(workspace_id, id) ON DELETE CASCADE,
    UNIQUE (workspace_id, connector_id)
);
