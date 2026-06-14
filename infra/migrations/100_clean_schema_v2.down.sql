-- Rollback 100: полный снос baseline-схемы v2.
-- Baseline применяется только на чистую БД, поэтому откат = полный teardown.

DROP SCHEMA IF EXISTS litellm CASCADE;

DROP VIEW IF EXISTS current_facts;

DROP TABLE IF EXISTS
  user_bots,
  draft_feedback,
  drafts,
  contact_profile_suggestions,
  contact_profiles,
  extraction_log,
  fact_relations,
  fact_occurrences,
  fact_embeddings,
  semantic_facts,
  episode_summaries,
  active_episodes,
  messages,
  message_states,
  audit_events,
  idempotency_records,
  unclaimed_chats,
  chat_settings,
  chats,
  ai_roles,
  telegram_business_connections,
  user_settings,
  tenants,
  users
CASCADE;

DROP FUNCTION IF EXISTS
  upsert_semantic_fact(text, text, text, text, text, text, real, text, text, timestamptz, vector, text),
  consolidate_facts(text),
  normalize_fact_text(text),
  bootstrap_user_settings(),
  bootstrap_chat_settings(),
  prevent_direct_global_health_insert(),
  view_chat_greeted_today(text, text, text),
  view_chat_paused_hours(text, text),
  view_open_commitments(text, text)
CASCADE;

DROP TYPE IF EXISTS
  chat_mode,
  decision,
  idempotency_operation,
  message_state,
  privacy_flag,
  risk_level
CASCADE;
