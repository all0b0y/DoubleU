package core

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"doubleu/backend-go/internal/aiclient"
)

// skillCurationConfig reads the autonomous-curator tuning from the environment
// (so thresholds are operator-set, not hardcoded policy). Sensible defaults keep
// it conservative: a skill must be genuinely used and disliked before a rewrite.
func skillCurationConfig() (interval time.Duration, minLoads, minNeg int) {
	return envDurationDefault("SKILL_CURATION_INTERVAL", 30*time.Minute),
		envIntDefault("SKILL_CURATION_MIN_LOADS", 15),
		envIntDefault("SKILL_CURATION_MIN_NEGATIVE", 3)
}

func envIntDefault(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDurationDefault(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// curateSkillsOnce runs one autonomous skill-improvement sweep: for skills of
// auto-improve roles that crossed the usage/negative thresholds since their last
// curation, it asks the AI to rewrite the body and applies the result as a
// revertible 'curator' version, then notifies the owner. It is a no-op when the
// AI client cannot improve skills; each skill is best-effort and isolated.
func (s *Service) curateSkillsOnce(ctx context.Context) {
	improver, ok := s.ai.(aiclient.ImproveSkiller)
	if !ok {
		return
	}
	_, minLoads, minNeg := skillCurationConfig()
	for _, c := range s.repository.SkillsDueForCuration(minLoads, minNeg, 20) {
		signal := fmt.Sprintf(
			"Загрузок с прошлого улучшения: %d. Понравилось владельцу: %d. Не понравилось: %d.",
			c.RecentLoads, c.RecentPositive, c.RecentNegative,
		)
		improved, err := improver.ImproveSkill(ctx, aiclient.ImproveSkillRequest{
			Name: c.Name, Description: c.Description, Body: c.Body, Signal: signal,
		})
		if err != nil {
			s.logger.Warn("autonomous skill curation failed",
				"tenant_id", c.TenantID, "skill_id", c.SkillID, "error", err)
			continue
		}
		body := strings.TrimSpace(improved.Body)
		// Skip empty or unchanged rewrites so we neither corrupt nor churn versions.
		if body == "" || body == strings.TrimSpace(c.Body) {
			continue
		}
		if err := s.repository.ApplyCuratedSkill(c.TenantID, c.SkillID, body); err != nil {
			s.logger.Warn("apply curated skill failed",
				"tenant_id", c.TenantID, "skill_id", c.SkillID, "error", err)
			continue
		}
		s.logger.Info("skill auto-improved",
			"tenant_id", c.TenantID, "skill_id", c.SkillID, "name", c.Name)
		if s.notify != nil {
			s.notify(c.TenantID, "skill_curated", map[string]any{
				"skill_id": c.SkillID, "name": c.Name,
			})
		}
	}
}
