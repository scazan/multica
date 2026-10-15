-- Asana integration: per-workspace token-based connections that mirror Asana
-- tasks into Multica issues so agents can own them. Modeled on the VCS
-- connection tables (migration 216) rather than the channel tables: Asana is a
-- task tracker reached over signed webhooks, not a chat surface with a
-- long-lived connection.
--
-- asana_connection       one Asana workspace reachable with one access token.
-- asana_project_binding  one Asana project mirrored into this Multica
--                        workspace; owns the Asana webhook and the agent /
--                        status mapping.
-- asana_task_link        Asana task <-> Multica issue.
-- asana_comment_link     Asana story <-> Multica comment, in either
--                        direction, so a comment we posted is never mirrored
--                        back as a new one.
--
-- Secrets are stored as base64-encoded secretbox ciphertext (never plaintext)
-- under the MULTICA_ASANA_SECRET_KEY box wired in cmd/server/router.go.
--
-- Per the project migration rules these tables carry NO foreign keys or
-- cascades: dependent cleanup is done in application code
-- (DeleteAsanaConnection, DeleteWorkspace, DeleteIssue). The inline UNIQUE /
-- PRIMARY KEY constraints stay — they back the ON CONFLICT upsert targets in
-- asana.sql. Secondary indexes live in follow-up single-statement CREATE INDEX
-- CONCURRENTLY migrations (584-586).

CREATE TABLE IF NOT EXISTS asana_connection (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id           UUID NOT NULL,
    asana_workspace_gid    TEXT NOT NULL,
    asana_workspace_name   TEXT NOT NULL DEFAULT '',
    -- The Asana user the token belongs to. Outbound writes show up as this
    -- user, and inbound sync skips stories it authored (echo suppression).
    asana_user_gid         TEXT NOT NULL,
    asana_user_name        TEXT NOT NULL DEFAULT '',
    asana_user_email       TEXT NOT NULL DEFAULT '',
    access_token_encrypted TEXT NOT NULL,
    connected_by_id        UUID,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, asana_workspace_gid)
);

CREATE TABLE IF NOT EXISTS asana_project_binding (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL,
    connection_id            UUID NOT NULL,
    asana_project_gid        TEXT NOT NULL,
    asana_project_name       TEXT NOT NULL DEFAULT '',
    -- Optional Multica project mirrored issues are filed under.
    project_id               UUID,
    -- Agent used when a task carries no agent custom-field value.
    default_agent_id         UUID,
    -- Asana custom field (enum) whose option name selects the agent.
    agent_field_gid          TEXT NOT NULL DEFAULT '',
    -- Asana section gid -> Multica issue status.
    status_map               JSONB NOT NULL DEFAULT '{}'::jsonb,
    webhook_gid              TEXT NOT NULL DEFAULT '',
    -- Set once Asana completes the X-Hook-Secret handshake.
    webhook_secret_encrypted TEXT NOT NULL DEFAULT '',
    status                   TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_by_id            UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, asana_project_gid)
);

CREATE TABLE IF NOT EXISTS asana_task_link (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL,
    binding_id        UUID NOT NULL,
    asana_task_gid    TEXT NOT NULL,
    issue_id          UUID NOT NULL,
    -- Asana's modified_at for the last version we applied; guards against an
    -- out-of-order webhook redelivery regressing the issue.
    asana_modified_at TIMESTAMPTZ,
    last_synced_at    TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (binding_id, asana_task_gid),
    UNIQUE (issue_id)
);

CREATE TABLE IF NOT EXISTS asana_comment_link (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    task_link_id    UUID NOT NULL,
    comment_id      UUID NOT NULL,
    asana_story_gid TEXT NOT NULL,
    direction       TEXT NOT NULL
        CHECK (direction IN ('inbound', 'outbound')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asana_story_gid),
    UNIQUE (comment_id)
);
