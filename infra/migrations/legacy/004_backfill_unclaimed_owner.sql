-- Migration: fix unclaimed_chats rows and associated chats/messages that were stored
-- under a fake tenant ID (tenant:<connectionID>) instead of the real owner
-- (tenant:user:<userID>). This happened when a message arrived before the
-- business_connection update was registered, causing NormalizeUpdate to fall back
-- to a connection-based tenant that was never correct.

-- Step 1: fix unclaimed_chats.owner_tenant_id where it points to a connection-based fake tenant.
UPDATE unclaimed_chats uc
SET owner_tenant_id = bc.tenant_id
FROM telegram_business_connections bc
WHERE uc.business_connection_id = bc.telegram_connection_id
  AND bc.tenant_id LIKE 'tenant:user:%'
  AND (uc.owner_tenant_id IS NULL
       OR uc.owner_tenant_id NOT LIKE 'tenant:user:%');

-- Step 2: move chats + messages from the fake tenant to the real owner tenant.
-- The fake tenant ID is deterministically 'tenant:' || telegram_connection_id.
-- Order matters: insert new chat row → re-point messages → delete old chat (to avoid
-- losing messages via the ON DELETE CASCADE FK on messages → chats).
DO $$
DECLARE
  r RECORD;
BEGIN
  FOR r IN
    SELECT c.tenant_id      AS bad_tenant,
           c.chat_id,
           c.telegram_chat_id,
           c.chat_type,
           c.display_name,
           c.status,
           c.created_at,
           c.updated_at,
           bc.tenant_id     AS good_tenant
    FROM   chats c
    JOIN   telegram_business_connections bc
           ON c.tenant_id = 'tenant:' || bc.telegram_connection_id
    WHERE  c.status = 'unclaimed'
      AND  bc.tenant_id LIKE 'tenant:user:%'
  LOOP
    -- Insert under the correct tenant; skip if a row already exists there.
    INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type,
                       display_name, status, created_at, updated_at)
    VALUES (r.good_tenant, r.chat_id, r.telegram_chat_id, r.chat_type,
            r.display_name, r.status, r.created_at, r.updated_at)
    ON CONFLICT DO NOTHING;

    -- Re-point messages to the new tenant before deleting the old chat row,
    -- so the CASCADE on chats DELETE does not remove them.
    UPDATE messages
    SET    tenant_id = r.good_tenant
    WHERE  tenant_id = r.bad_tenant
      AND  chat_id   = r.chat_id;

    -- Now safe to remove the orphaned chat row.
    DELETE FROM chats
    WHERE  tenant_id = r.bad_tenant
      AND  chat_id   = r.chat_id;
  END LOOP;
END $$;
