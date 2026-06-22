package contracts

// ToolCatalogEntry describes one native AI tool and its owner-visible policy.
type ToolCatalogEntry struct {
	Key            string       `json:"key"`
	Label          string       `json:"label"`
	Description    string       `json:"description"`
	Category       ToolCategory `json:"category"`
	DefaultEnabled bool         `json:"default_enabled"`
}

// ToolCatalog returns the Go mirror of the canonical Python native-tool catalog.
func ToolCatalog() []ToolCatalogEntry {
	return []ToolCatalogEntry{
		{Key: "memory.search_facts", Label: "Поиск фактов", Description: "Ищет сохранённые факты о собеседнике и договорённостях.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "memory.get_episode_summaries", Label: "Сводки прошлых разговоров", Description: "Читает краткие сводки предыдущих эпизодов общения.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "memory.get_recent_messages", Label: "Недавние сообщения", Description: "Получает последние сообщения чата, когда видимого контекста мало.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "memory.get_contact_profile", Label: "Профиль собеседника", Description: "Читает сохранённые имя, отношения, тон и обращение к собеседнику.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "memory.save_fact", Label: "Сохранение фактов", Description: "Сохраняет устойчивые факты и договорённости в память чата.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "memory.suggest_profile_update", Label: "Предложения к профилю", Description: "Предлагает владельцу обновить профиль собеседника.", Category: ToolCategoryRead, DefaultEnabled: true},
		{Key: "interaction.present_buttons", Label: "Кнопки для собеседника", Description: "Задаёт собеседнику вопрос с вариантами ответа или подтверждением.", Category: ToolCategoryInteraction, DefaultEnabled: false},
	}
}

// ToolEnabled resolves a role override while preserving the catalog default.
func ToolEnabled(settings map[string]bool, key string) bool {
	if enabled, ok := settings[key]; ok {
		return enabled
	}
	for _, tool := range ToolCatalog() {
		if tool.Key == key {
			return tool.DefaultEnabled
		}
	}
	return false
}

// IsKnownToolKey reports whether key belongs to the native-tool catalog.
func IsKnownToolKey(key string) bool {
	for _, tool := range ToolCatalog() {
		if tool.Key == key {
			return true
		}
	}
	return false
}
