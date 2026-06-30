CREATE TABLE IF NOT EXISTS interaction_forms (
    id bigserial PRIMARY KEY,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    bot_id bigint NULL,
    questions jsonb NOT NULL DEFAULT '[]'::jsonb,
    current_index int NOT NULL DEFAULT 0,
    answers jsonb NOT NULL DEFAULT '[]'::jsonb,
    status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'completed', 'cancelled')),
    source_message_id text NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CONSTRAINT interaction_forms_chat_fk
        FOREIGN KEY (tenant_id, chat_id)
        REFERENCES chats (tenant_id, chat_id)
        ON DELETE CASCADE
);

-- At most one active questionnaire per chat: lookups are by (tenant, chat,
-- status='active') and the flow assumes a single in-progress form.
CREATE UNIQUE INDEX IF NOT EXISTS interaction_forms_one_active_idx
    ON interaction_forms (tenant_id, chat_id)
    WHERE status = 'active';
