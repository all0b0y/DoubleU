package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRoleContextPolicyRequired = errors.New("role_context_policy is required")
	ErrInvalidRoleContextPolicy  = errors.New("invalid role_context_policy")
	ErrUserBotNotFound           = errors.New("user bot not found")
)

const roleContextColumns = `id, tenant_id, bot_id, ai_role_id, created_at, last_activated_at, archived_at`

func validateRoleContextPolicy(policy RoleContextPolicy) error {
	if policy == "" {
		return ErrRoleContextPolicyRequired
	}
	if !policy.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidRoleContextPolicy, policy)
	}
	return nil
}

func normalizeRoleContextRoleID(roleID *int64) *int64 {
	if roleID == nil || *roleID == 0 {
		return nil
	}
	v := *roleID
	return &v
}

func nullableRoleContextRoleID(roleID *int64) sql.NullInt64 {
	if roleID == nil || *roleID == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *roleID, Valid: true}
}

func roleContextRoleIDValue(roleID *int64) int64 {
	if roleID == nil {
		return 0
	}
	return *roleID
}

func sameRoleContextRoleID(a, b *int64) bool {
	a = normalizeRoleContextRoleID(a)
	b = normalizeRoleContextRoleID(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func scanRoleContext(sc rowScanner) (RoleContextRecord, error) {
	var rec RoleContextRecord
	var roleID sql.NullInt64
	var archivedAt sql.NullTime
	if err := sc.Scan(
		&rec.ID,
		&rec.TenantID,
		&rec.BotID,
		&roleID,
		&rec.CreatedAt,
		&rec.LastActivatedAt,
		&archivedAt,
	); err != nil {
		return RoleContextRecord{}, err
	}
	if roleID.Valid {
		rec.AIRoleID = &roleID.Int64
	}
	if archivedAt.Valid {
		rec.ArchivedAt = &archivedAt.Time
	}
	return rec, nil
}

// EnsureBotRoleContext activates a reusable memory context for one bot role.
// roleID nil or 0 means the default no-role persona and is stored as SQL NULL.
func (r *Repository) EnsureBotRoleContext(tenantID string, botID int64, roleID *int64, policy RoleContextPolicy) (RoleContextRecord, error) {
	roleID = normalizeRoleContextRoleID(roleID)
	if tenantID == "" || botID == 0 {
		return RoleContextRecord{}, fmt.Errorf("tenant_id and bot_id are required")
	}
	if err := validateRoleContextPolicy(policy); err != nil {
		return RoleContextRecord{}, err
	}
	if r.db != nil {
		tx, err := r.db.Begin()
		if err != nil {
			return RoleContextRecord{}, err
		}
		rec, err := r.ensureBotRoleContextTx(tx, tenantID, botID, roleID, policy)
		if err != nil {
			_ = tx.Rollback()
			return RoleContextRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return RoleContextRecord{}, err
		}
		return rec, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureBotRoleContextMemoryLocked(tenantID, botID, roleID, policy, time.Now().UTC())
}

// ActiveBotRoleContext returns the bot's currently activated memory context.
func (r *Repository) ActiveBotRoleContext(tenantID string, botID int64) (RoleContextRecord, bool) {
	if r.db != nil {
		rec, err := scanRoleContext(r.db.QueryRow(
			`SELECT `+roleContextColumns+`
			   FROM bot_role_contexts
			  WHERE id = (
			        SELECT active_role_context_id
			          FROM user_bots
			         WHERE tenant_id = $1 AND bot_id = $2
			  )`,
			tenantID, botID,
		))
		return rec, err == nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	bot, ok := r.userBots[botStoreKey(tenantID, botID)]
	if !ok || bot.ActiveRoleContextID == 0 {
		return RoleContextRecord{}, false
	}
	rec, ok := r.roleContexts[bot.ActiveRoleContextID]
	return rec, ok
}

// SetUserBotRoleWithContextPolicy binds a role and explicitly chooses the bot
// memory context to activate. It is the policy-aware replacement for future HTTP
// role updates; SetUserBotRole is kept for the legacy runtime path until the
// contract slice wires the required policy through the API.
func (r *Repository) SetUserBotRoleWithContextPolicy(tenantID string, botID, roleID int64, policy RoleContextPolicy) (RoleContextRecord, error) {
	normalizedRoleID := normalizeRoleContextRoleID(&roleID)
	if tenantID == "" || botID == 0 {
		return RoleContextRecord{}, fmt.Errorf("tenant_id and bot_id are required")
	}
	if err := validateRoleContextPolicy(policy); err != nil {
		return RoleContextRecord{}, err
	}
	if r.db != nil {
		tx, err := r.db.Begin()
		if err != nil {
			return RoleContextRecord{}, err
		}
		rec, err := r.ensureBotRoleContextTx(tx, tenantID, botID, normalizedRoleID, policy)
		if err != nil {
			_ = tx.Rollback()
			return RoleContextRecord{}, err
		}
		if _, err := tx.Exec(
			`UPDATE user_bots
			    SET ai_role_id = $3,
			        active_role_context_id = $4,
			        updated_at = now()
			  WHERE tenant_id = $1 AND bot_id = $2`,
			tenantID, botID, nullableRoleContextRoleID(normalizedRoleID), rec.ID,
		); err != nil {
			_ = tx.Rollback()
			return RoleContextRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return RoleContextRecord{}, err
		}
		return rec, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.ensureBotRoleContextMemoryLocked(tenantID, botID, normalizedRoleID, policy, time.Now().UTC())
	if err != nil {
		return RoleContextRecord{}, err
	}
	bot := r.userBots[botStoreKey(tenantID, botID)]
	bot.AIRoleID = roleContextRoleIDValue(normalizedRoleID)
	bot.ActiveRoleContextID = rec.ID
	r.userBots[botStoreKey(tenantID, botID)] = bot
	return rec, nil
}

func (r *Repository) ensureBotRoleContextTx(tx *sql.Tx, tenantID string, botID int64, roleID *int64, policy RoleContextPolicy) (RoleContextRecord, error) {
	var exists int
	if err := tx.QueryRow(
		`SELECT 1 FROM user_bots WHERE tenant_id = $1 AND bot_id = $2 FOR UPDATE`,
		tenantID, botID,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoleContextRecord{}, ErrUserBotNotFound
		}
		return RoleContextRecord{}, err
	}

	var rec RoleContextRecord
	var err error
	if policy == RoleContextPolicyReuse {
		rec, err = findReusableRoleContextTx(tx, tenantID, botID, roleID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return RoleContextRecord{}, err
		}
		if err == nil {
			rec, err = touchRoleContextTx(tx, rec.ID)
			if err != nil {
				return RoleContextRecord{}, err
			}
			if err := activateRoleContextTx(tx, tenantID, botID, rec.ID); err != nil {
				return RoleContextRecord{}, err
			}
			return rec, nil
		}
	}

	rec, err = insertRoleContextTx(tx, tenantID, botID, roleID)
	if err != nil {
		return RoleContextRecord{}, err
	}
	if err := activateRoleContextTx(tx, tenantID, botID, rec.ID); err != nil {
		return RoleContextRecord{}, err
	}
	return rec, nil
}

func findReusableRoleContextTx(tx *sql.Tx, tenantID string, botID int64, roleID *int64) (RoleContextRecord, error) {
	if roleID == nil {
		return scanRoleContext(tx.QueryRow(
			`SELECT `+roleContextColumns+`
			   FROM bot_role_contexts
			  WHERE tenant_id = $1
			    AND bot_id = $2
			    AND ai_role_id IS NULL
			    AND archived_at IS NULL
			  ORDER BY last_activated_at DESC, id DESC
			  LIMIT 1`,
			tenantID, botID,
		))
	}
	return scanRoleContext(tx.QueryRow(
		`SELECT `+roleContextColumns+`
		   FROM bot_role_contexts
		  WHERE tenant_id = $1
		    AND bot_id = $2
		    AND ai_role_id = $3
		    AND archived_at IS NULL
		  ORDER BY last_activated_at DESC, id DESC
		  LIMIT 1`,
		tenantID, botID, *roleID,
	))
}

func touchRoleContextTx(tx *sql.Tx, id int64) (RoleContextRecord, error) {
	return scanRoleContext(tx.QueryRow(
		`UPDATE bot_role_contexts
		    SET last_activated_at = now()
		  WHERE id = $1
		  RETURNING `+roleContextColumns,
		id,
	))
}

func insertRoleContextTx(tx *sql.Tx, tenantID string, botID int64, roleID *int64) (RoleContextRecord, error) {
	return scanRoleContext(tx.QueryRow(
		`INSERT INTO bot_role_contexts (tenant_id, bot_id, ai_role_id)
		 VALUES ($1, $2, $3)
		 RETURNING `+roleContextColumns,
		tenantID, botID, nullableRoleContextRoleID(roleID),
	))
}

func activateRoleContextTx(tx *sql.Tx, tenantID string, botID, roleContextID int64) error {
	_, err := tx.Exec(
		`UPDATE user_bots
		    SET active_role_context_id = $3,
		        updated_at = now()
		  WHERE tenant_id = $1 AND bot_id = $2`,
		tenantID, botID, roleContextID,
	)
	return err
}

func (r *Repository) ensureBotRoleContextMemoryLocked(tenantID string, botID int64, roleID *int64, policy RoleContextPolicy, now time.Time) (RoleContextRecord, error) {
	key := botStoreKey(tenantID, botID)
	bot, ok := r.userBots[key]
	if !ok {
		return RoleContextRecord{}, ErrUserBotNotFound
	}
	if policy == RoleContextPolicyReuse {
		if rec, ok := r.findReusableRoleContextMemoryLocked(tenantID, botID, roleID); ok {
			rec.LastActivatedAt = now
			r.roleContexts[rec.ID] = rec
			bot.ActiveRoleContextID = rec.ID
			r.userBots[key] = bot
			return rec, nil
		}
	}
	r.roleContextSeq++
	rec := RoleContextRecord{
		ID:              r.roleContextSeq,
		TenantID:        tenantID,
		BotID:           botID,
		AIRoleID:        normalizeRoleContextRoleID(roleID),
		CreatedAt:       now,
		LastActivatedAt: now,
	}
	r.roleContexts[rec.ID] = rec
	bot.ActiveRoleContextID = rec.ID
	r.userBots[key] = bot
	return rec, nil
}

func (r *Repository) findReusableRoleContextMemoryLocked(tenantID string, botID int64, roleID *int64) (RoleContextRecord, bool) {
	var best RoleContextRecord
	found := false
	for _, rec := range r.roleContexts {
		if rec.TenantID != tenantID ||
			rec.BotID != botID ||
			rec.ArchivedAt != nil ||
			!sameRoleContextRoleID(rec.AIRoleID, roleID) {
			continue
		}
		if !found ||
			rec.LastActivatedAt.After(best.LastActivatedAt) ||
			(rec.LastActivatedAt.Equal(best.LastActivatedAt) && rec.ID > best.ID) {
			best = rec
			found = true
		}
	}
	return best, found
}
