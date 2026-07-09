package store

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"doubleu/backend-go/internal/contracts"
)

// SaveAIRole creates or updates (by tenant+name) an owner-authored AI role and
// returns the stored record with its ID populated.
func (r *Repository) SaveAIRole(role AIRoleRecord) AIRoleRecord {
	if r.db != nil {
		telegramUserID := ""
		if strings.HasPrefix(role.TenantID, "tenant:user:") {
			telegramUserID = strings.TrimPrefix(role.TenantID, "tenant:user:")
		}
		r.ensureTenant(role.TenantID, telegramUserID)
		err := r.db.QueryRow(
			`INSERT INTO ai_roles (
			   tenant_id, name, description, speaking_style, goals, constraints_text,
			   context_notes, custom_fields, rich_formatting_enabled, streaming_enabled,
			   speak_as_owner, model_tier, model_name, tool_settings, auto_learn_profile,
			   auto_improve_skills
			 )
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			 ON CONFLICT (tenant_id, name) DO UPDATE SET
			   description      = EXCLUDED.description,
			   speaking_style   = EXCLUDED.speaking_style,
			   goals            = EXCLUDED.goals,
			   constraints_text = EXCLUDED.constraints_text,
			   context_notes    = EXCLUDED.context_notes,
			   custom_fields    = EXCLUDED.custom_fields,
			   rich_formatting_enabled = COALESCE(EXCLUDED.rich_formatting_enabled, ai_roles.rich_formatting_enabled),
			   streaming_enabled       = COALESCE(EXCLUDED.streaming_enabled, ai_roles.streaming_enabled),
			   speak_as_owner   = EXCLUDED.speak_as_owner,
			   model_tier       = EXCLUDED.model_tier,
			   model_name       = EXCLUDED.model_name,
			   tool_settings    = EXCLUDED.tool_settings,
			   auto_learn_profile = EXCLUDED.auto_learn_profile,
			   auto_improve_skills = EXCLUDED.auto_improve_skills,
			   updated_at       = now()
			 RETURNING id`,
			role.TenantID, role.Name, role.Description, role.SpeakingStyle, role.Goals, role.Constraints, role.Context,
			encodeRoleCustomFields(role.CustomFields), nullableBoolArg(role.RichFormattingEnabled), nullableBoolArg(role.StreamingEnabled),
			role.SpeakAsOwner, role.ModelTier, role.ModelName, encodeRoleToolSettings(role.ToolSettings), role.AutoLearnProfile,
			role.AutoImproveSkills,
		).Scan(&role.ID)
		if err != nil {
			r.logWriteError("save_ai_role", role.TenantID, "", err)
			return AIRoleRecord{}
		}
		return cloneAIRoleRecord(role)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.aiRoles {
		if existing.TenantID == role.TenantID && strings.EqualFold(existing.Name, role.Name) {
			role.ID = id
			r.aiRoles[id] = cloneAIRoleRecord(role)
			return cloneAIRoleRecord(role)
		}
	}
	r.aiRoleSeq++
	role.ID = r.aiRoleSeq
	r.aiRoles[role.ID] = cloneAIRoleRecord(role)
	return cloneAIRoleRecord(role)
}

// aiRoleColumns is the shared SELECT projection for ai_roles, kept in one place
// so every read returns the same fields, including both JSONB-backed settings.
const aiRoleColumns = `id, tenant_id, name, description, speaking_style, goals, constraints_text, COALESCE(context_notes,''), COALESCE(custom_fields,'[]'::jsonb), rich_formatting_enabled, streaming_enabled, speak_as_owner, COALESCE(model_tier,'balanced'), COALESCE(model_name,''), COALESCE(tool_settings,'{}'::jsonb), COALESCE(auto_learn_profile, true), COALESCE(auto_improve_skills, false)`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanAIRole reads one ai_roles row in aiRoleColumns order and decodes JSONB fields.
func scanAIRole(s rowScanner) (AIRoleRecord, error) {
	var role AIRoleRecord
	var customFields []byte
	var toolSettings []byte
	var richFormatting sql.NullBool
	var streaming sql.NullBool
	if err := s.Scan(&role.ID, &role.TenantID, &role.Name, &role.Description,
		&role.SpeakingStyle, &role.Goals, &role.Constraints, &role.Context, &customFields,
		&richFormatting, &streaming, &role.SpeakAsOwner, &role.ModelTier, &role.ModelName, &toolSettings,
		&role.AutoLearnProfile, &role.AutoImproveSkills); err != nil {
		return AIRoleRecord{}, err
	}
	role.CustomFields = decodeRoleCustomFields(customFields)
	role.ToolSettings = decodeRoleToolSettings(toolSettings)
	role.RichFormattingEnabled = boolPtrFromNull(richFormatting)
	role.StreamingEnabled = boolPtrFromNull(streaming)
	return role, nil
}

// decodeRoleCustomFields unmarshals the custom_fields jsonb; a malformed or
// empty value yields nil so callers always get a usable (possibly empty) slice.
func decodeRoleCustomFields(raw []byte) []contracts.AIRoleCustomField {
	if len(raw) == 0 {
		return nil
	}
	var out []contracts.AIRoleCustomField
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// encodeRoleCustomFields marshals custom fields for storage; nil/empty becomes
// the jsonb empty array so the NOT NULL column always has a valid value.
func encodeRoleCustomFields(fields []contracts.AIRoleCustomField) []byte {
	if len(fields) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// decodeRoleToolSettings unmarshals tool overrides; malformed values degrade
// to an empty map so every tool falls back to its catalog default.
func decodeRoleToolSettings(raw []byte) map[string]bool {
	out := make(map[string]bool)
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return make(map[string]bool)
	}
	return out
}

// encodeRoleToolSettings stores explicit overrides as a JSON object.
func encodeRoleToolSettings(settings map[string]bool) []byte {
	if len(settings) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func cloneRoleToolSettings(settings map[string]bool) map[string]bool {
	cloned := make(map[string]bool, len(settings))
	for key, enabled := range settings {
		cloned[key] = enabled
	}
	return cloned
}

func cloneAIRoleRecord(role AIRoleRecord) AIRoleRecord {
	role.ToolSettings = cloneRoleToolSettings(role.ToolSettings)
	return role
}

// AIRoles returns all roles defined by the tenant, ordered by name.
func (r *Repository) AIRoles(tenantID string) []AIRoleRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return nil
		}
		defer rows.Close()
		return scanAIRoles(rows)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]AIRoleRecord, 0, len(r.aiRoles))
	for _, role := range r.aiRoles {
		if role.TenantID == tenantID {
			result = append(result, cloneAIRoleRecord(role))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// AIRoleByName returns the tenant's role with the given name (case-insensitive).
func (r *Repository) AIRoleByName(tenantID, name string) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 AND lower(name) = lower($2)`,
			tenantID, name,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, role := range r.aiRoles {
		if role.TenantID == tenantID && strings.EqualFold(role.Name, name) {
			return cloneAIRoleRecord(role), true
		}
	}
	return AIRoleRecord{}, false
}

// AssignAIRoleToChat binds a role to a chat; roleID 0 clears the assignment.
// The chat_settings row is created with defaults when it does not exist yet so
// the assignment survives for chats the owner has not configured otherwise.
func (r *Repository) AssignAIRoleToChat(tenantID, chatID string, roleID int64) {
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO chat_settings (tenant_id, chat_id, mode, ai_role_id)
			 VALUES ($1, $2, 'manual'::chat_mode, NULLIF($3, 0))
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   ai_role_id = NULLIF($3, 0),
			   updated_at = now()`,
			tenantID, chatID, roleID,
		)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if roleID == 0 {
		delete(r.chatAIRoleIDs, chatKey(tenantID, chatID))
		return
	}
	r.chatAIRoleIDs[chatKey(tenantID, chatID)] = roleID
}

// AIRoleForChat resolves the role assigned to a chat, if any.
func (r *Repository) AIRoleForChat(tenantID, chatID string) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT ar.id, ar.tenant_id, ar.name, ar.description, ar.speaking_style, ar.goals,
			        ar.constraints_text, COALESCE(ar.context_notes,''), COALESCE(ar.custom_fields,'[]'::jsonb),
			        ar.rich_formatting_enabled, ar.streaming_enabled, ar.speak_as_owner,
			        COALESCE(ar.model_tier,'balanced'), COALESCE(ar.model_name,''), COALESCE(ar.tool_settings,'{}'::jsonb),
			        COALESCE(ar.auto_learn_profile, true), COALESCE(ar.auto_improve_skills, false)
			   FROM chat_settings cs
			   JOIN ai_roles ar ON ar.id = cs.ai_role_id
			  WHERE cs.tenant_id = $1 AND cs.chat_id = $2`,
			tenantID, chatID,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	roleID, ok := r.chatAIRoleIDs[chatKey(tenantID, chatID)]
	if !ok {
		return AIRoleRecord{}, false
	}
	role, ok := r.aiRoles[roleID]
	return cloneAIRoleRecord(role), ok
}

func scanAIRoles(rows *sql.Rows) []AIRoleRecord {
	result := make([]AIRoleRecord, 0)
	for rows.Next() {
		if role, err := scanAIRole(rows); err == nil {
			result = append(result, role)
		}
	}
	return result
}

// AIRoleByID returns the tenant's role with the given ID.
func (r *Repository) AIRoleByID(tenantID string, roleID int64) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 AND id = $2`,
			tenantID, roleID,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	role, ok := r.aiRoles[roleID]
	if !ok || role.TenantID != tenantID {
		return AIRoleRecord{}, false
	}
	return cloneAIRoleRecord(role), true
}

// UpdateAIRole rewrites an existing role addressed by ID. Unlike SaveAIRole
// (which upserts by tenant+name and would create a second role on rename),
// this is the rename-safe path. Returns false when the role does not exist.
func (r *Repository) UpdateAIRole(role AIRoleRecord) bool {
	if r.db != nil {
		res, err := r.db.Exec(
			`UPDATE ai_roles
			    SET name = $3, description = $4, speaking_style = $5,
			        goals = $6, constraints_text = $7, context_notes = $8,
			        custom_fields = $9, rich_formatting_enabled = $10,
			        streaming_enabled = $11, speak_as_owner = $12,
			        model_tier = $13, model_name = $14, tool_settings = $15,
			        auto_learn_profile = $16, auto_improve_skills = $17, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			role.TenantID, role.ID, role.Name, role.Description,
			role.SpeakingStyle, role.Goals, role.Constraints, role.Context,
			encodeRoleCustomFields(role.CustomFields),
			nullableBoolArg(role.RichFormattingEnabled), nullableBoolArg(role.StreamingEnabled),
			role.SpeakAsOwner, role.ModelTier, role.ModelName, encodeRoleToolSettings(role.ToolSettings),
			role.AutoLearnProfile, role.AutoImproveSkills,
		)
		if err != nil {
			r.logWriteError("update_ai_role", role.TenantID, "", err)
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.aiRoles[role.ID]
	if !ok || existing.TenantID != role.TenantID {
		return false
	}
	r.aiRoles[role.ID] = cloneAIRoleRecord(role)
	return true
}

// UpdateAIRoleToolSettings replaces only native-tool overrides for one role.
func (r *Repository) UpdateAIRoleToolSettings(tenantID string, roleID int64, settings map[string]bool) bool {
	settings = cloneRoleToolSettings(settings)
	if r.db != nil {
		res, err := r.db.Exec(
			`UPDATE ai_roles SET tool_settings = $3, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, roleID, encodeRoleToolSettings(settings),
		)
		if err != nil {
			r.logWriteError("update_ai_role_tools", tenantID, "", err)
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	role, ok := r.aiRoles[roleID]
	if !ok || role.TenantID != tenantID {
		return false
	}
	role.ToolSettings = settings
	r.aiRoles[roleID] = role
	return true
}

// DeleteAIRole removes a role and clears its assignment from every chat in the
// same transaction, so no chat_settings row is left pointing at a missing role.
// Deleting an absent role is a no-op (idempotent).
func (r *Repository) DeleteAIRole(tenantID string, roleID int64) error {
	r.mu.Lock()
	if role, ok := r.aiRoles[roleID]; ok && role.TenantID == tenantID {
		delete(r.aiRoles, roleID)
		for key, id := range r.chatAIRoleIDs {
			if id == roleID {
				delete(r.chatAIRoleIDs, key)
			}
		}
	}
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE chat_settings SET ai_role_id = NULL, updated_at = now()
		  WHERE tenant_id = $1 AND ai_role_id = $2`,
		tenantID, roleID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("delete_ai_role_unassign", tenantID, "", err)
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM ai_roles WHERE tenant_id = $1 AND id = $2`,
		tenantID, roleID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("delete_ai_role", tenantID, "", err)
		return err
	}
	return tx.Commit()
}
