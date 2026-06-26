CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id bigserial PRIMARY KEY,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    bot_id bigint NULL,
    kind text NOT NULL CHECK (kind IN ('reminder', 'message', 'ai_followup')),
    target text NOT NULL CHECK (target IN ('owner', 'contact')),
    fire_at timestamptz NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'cancelled', 'failed')),
    created_by text NOT NULL CHECK (created_by IN ('ai', 'owner')),
    idempotency_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz NULL,
    CONSTRAINT scheduled_jobs_chat_fk
        FOREIGN KEY (tenant_id, chat_id)
        REFERENCES chats (tenant_id, chat_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS scheduled_jobs_status_fire_at_idx
    ON scheduled_jobs (status, fire_at);
