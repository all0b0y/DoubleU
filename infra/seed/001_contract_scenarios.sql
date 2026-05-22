BEGIN;

INSERT INTO users (id, telegram_user_id, email) VALUES
  ('user_tenant_a', '900001', 'tenant_a@example.test'),
  ('user_tenant_b', '900002', 'tenant_b@example.test')
ON CONFLICT (id) DO NOTHING;

INSERT INTO tenants (id, owner_user_id, name) VALUES
  ('tenant_a', 'user_tenant_a', 'Tenant A'),
  ('tenant_b', 'user_tenant_b', 'Tenant B')
ON CONFLICT (id) DO NOTHING;

INSERT INTO telegram_business_connections (
  id,
  tenant_id,
  telegram_connection_id,
  telegram_user_id,
  permissions_json,
  is_active
) VALUES
  ('tbc_tenant_a', 'tenant_a', 'biz_conn_tenant_a', '900001', '{"can_reply": true}', true),
  ('tbc_tenant_b', 'tenant_b', 'biz_conn_tenant_b', '900002', '{"can_reply": true}', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO assistant_profiles (
  id,
  tenant_id,
  name,
  default_mode,
  default_model,
  style_config_json,
  privacy_config_json
) VALUES
  ('profile_tenant_a', 'tenant_a', 'Tenant A Assistant', 'semi_auto', 'fast', '{"tone": "concise"}', '{"memory": "allowed"}'),
  ('profile_tenant_b', 'tenant_b', 'Tenant B Assistant', 'manual', 'fast', '{"tone": "formal"}', '{"memory": "restricted"}')
ON CONFLICT (id) DO NOTHING;

INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name, relationship_type) VALUES
  ('tenant_a', 'disabled_chat', '100100', 'private', 'Disabled Chat', 'private'),
  ('tenant_a', 'no_memory_chat', '100101', 'private', 'No Memory Chat', 'client'),
  ('tenant_a', 'semi_auto_chat', '100102', 'private', 'Semi Auto Chat', 'client'),
  ('tenant_b', 'semi_auto_chat', '200102', 'private', 'Tenant B Semi Auto Chat', 'client')
ON CONFLICT (tenant_id, chat_id) DO NOTHING;

INSERT INTO chat_settings (tenant_id, chat_id, mode, privacy_flags, settings_json) VALUES
  ('tenant_a', 'disabled_chat', 'disabled', ARRAY['private']::privacy_flag[], '{"reason": "owner_disabled"}'),
  ('tenant_a', 'no_memory_chat', 'manual', ARRAY['no_memory', 'no_summary']::privacy_flag[], '{"memory": false}'),
  ('tenant_a', 'semi_auto_chat', 'semi_auto', ARRAY[]::privacy_flag[], '{"allow_low_risk_auto": true}'),
  ('tenant_b', 'semi_auto_chat', 'semi_auto', ARRAY['no_training']::privacy_flag[], '{"allow_low_risk_auto": true}')
ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
  mode = EXCLUDED.mode,
  privacy_flags = EXCLUDED.privacy_flags,
  settings_json = EXCLUDED.settings_json,
  updated_at = now();

INSERT INTO messages (
  tenant_id,
  chat_id,
  source_message_id,
  event_id,
  telegram_update_id,
  business_connection_id,
  sender_id,
  message_text,
  message_type,
  risk_level,
  received_at
) VALUES
  (
    'tenant_a',
    'semi_auto_chat',
    'low_risk_message',
    'evt_low_risk_message',
    710001,
    'biz_conn_tenant_a',
    'sender_client_low',
    'Can we confirm the meeting for tomorrow at 10?',
    'text',
    'low',
    '2026-05-13T09:00:00Z'
  ),
  (
    'tenant_a',
    'semi_auto_chat',
    'high_risk_message',
    'evt_high_risk_message',
    710002,
    'biz_conn_tenant_a',
    'sender_client_high',
    'Can you guarantee the refund and sign the contract today?',
    'text',
    'high',
    '2026-05-13T09:05:00Z'
  ),
  (
    'tenant_a',
    'disabled_chat',
    'disabled_chat_message',
    'evt_disabled_chat_message',
    710003,
    'biz_conn_tenant_a',
    'sender_disabled',
    'This chat should not receive replies.',
    'text',
    'low',
    '2026-05-13T09:10:00Z'
  ),
  (
    'tenant_a',
    'no_memory_chat',
    'no_memory_chat_message',
    'evt_no_memory_chat_message',
    710004,
    'biz_conn_tenant_a',
    'sender_private',
    'Please do not store this sensitive context.',
    'text',
    'medium',
    '2026-05-13T09:15:00Z'
  )
ON CONFLICT (tenant_id, chat_id, source_message_id) DO NOTHING;

INSERT INTO message_states (tenant_id, chat_id, source_message_id, state, state_reason) VALUES
  ('tenant_a', 'semi_auto_chat', 'low_risk_message', 'received', 'seed inbound update'),
  ('tenant_a', 'semi_auto_chat', 'low_risk_message', 'policy_checked', 'low risk and semi-auto eligible'),
  ('tenant_a', 'semi_auto_chat', 'low_risk_message', 'decision_made', 'auto reply candidate'),
  ('tenant_a', 'semi_auto_chat', 'high_risk_message', 'received', 'seed inbound update'),
  ('tenant_a', 'semi_auto_chat', 'high_risk_message', 'policy_checked', 'high risk requires draft'),
  ('tenant_a', 'semi_auto_chat', 'high_risk_message', 'decision_made', 'draft only'),
  ('tenant_a', 'disabled_chat', 'disabled_chat_message', 'ignored', 'chat mode disabled'),
  ('tenant_a', 'no_memory_chat', 'no_memory_chat_message', 'policy_checked', 'no_memory blocks persistence beyond audit')
ON CONFLICT DO NOTHING;

INSERT INTO audit_events (
  tenant_id,
  chat_id,
  source_message_id,
  decision,
  risk_level,
  reason,
  privacy_flags,
  policy_snapshot
) VALUES
  (
    'tenant_a',
    'semi_auto_chat',
    'low_risk_message',
    'auto_reply',
    'low',
    'low risk and confidence above semi_auto_min',
    ARRAY[]::privacy_flag[],
    '{"chat_mode": "semi_auto", "confidence": 0.91, "threshold": 0.85}'
  ),
  (
    'tenant_a',
    'semi_auto_chat',
    'high_risk_message',
    'draft',
    'high',
    'high risk messages are draft only',
    ARRAY[]::privacy_flag[],
    '{"chat_mode": "semi_auto", "risk_policy": "draft_only"}'
  ),
  (
    'tenant_a',
    'disabled_chat',
    'disabled_chat_message',
    'ignore',
    'low',
    'chat mode disabled',
    ARRAY['private']::privacy_flag[],
    '{"chat_mode": "disabled"}'
  ),
  (
    'tenant_a',
    'no_memory_chat',
    'no_memory_chat_message',
    'draft',
    'medium',
    'manual mode and no_memory privacy flag',
    ARRAY['no_memory', 'no_summary']::privacy_flag[],
    '{"chat_mode": "manual", "memory_persisted": false}'
  )
ON CONFLICT DO NOTHING;

INSERT INTO idempotency_records (
  idempotency_key,
  operation,
  tenant_id,
  chat_id,
  source_message_id,
  decision_id,
  request_hash,
  response_json,
  completed_at
) VALUES
  (
    '710001:biz_conn_tenant_a',
    'receive_update',
    'tenant_a',
    'semi_auto_chat',
    'low_risk_message',
    NULL,
    'seed-receive-low-risk',
    '{"status": "accepted"}',
    now()
  ),
  (
    'tenant_a:semi_auto_chat:low_risk_message:decision_low_risk',
    'send_reply',
    'tenant_a',
    'semi_auto_chat',
    'low_risk_message',
    'decision_low_risk',
    'seed-send-low-risk',
    '{"status": "sent"}',
    now()
  ),
  (
    'approval:tenant_a:semi_auto_chat:high_risk_message:decision_high_risk',
    'approval_callback',
    'tenant_a',
    'semi_auto_chat',
    'high_risk_message',
    'decision_high_risk',
    'seed-approval-high-risk',
    '{"status": "pending_owner"}',
    NULL
  )
ON CONFLICT (idempotency_key) DO NOTHING;

INSERT INTO memory_facts (
  tenant_id,
  chat_id,
  scope,
  fact_type,
  fact_text,
  confidence,
  source_message_id,
  metadata
) VALUES
  (
    'tenant_a',
    'semi_auto_chat',
    'conversation',
    'preference',
    'Client prefers morning meetings.',
    0.880,
    'low_risk_message',
    '{"seed": true}'
  )
ON CONFLICT DO NOTHING;

INSERT INTO memory_embeddings (
  tenant_id,
  chat_id,
  memory_fact_id,
  embedding_model,
  embedding_dimensions,
  embedding,
  content_hash
)
SELECT
  tenant_id,
  chat_id,
  id,
  'placeholder',
  3,
  ARRAY[0.10, 0.20, 0.30]::float8[],
  'seed-memory-fact-embedding'
FROM memory_facts
WHERE tenant_id = 'tenant_a'
  AND chat_id = 'semi_auto_chat'
  AND source_message_id = 'low_risk_message'
ON CONFLICT DO NOTHING;

INSERT INTO summaries (
  tenant_id,
  chat_id,
  summary_type,
  period_start,
  period_end,
  content,
  source_message_ids,
  privacy_flags
) VALUES
  (
    'tenant_a',
    'semi_auto_chat',
    'conversation',
    '2026-05-13T00:00:00Z',
    '2026-05-13T23:59:59Z',
    'Client is coordinating a meeting and prefers direct confirmation.',
    ARRAY['low_risk_message']::text[],
    ARRAY[]::privacy_flag[]
  )
ON CONFLICT DO NOTHING;

COMMIT;
