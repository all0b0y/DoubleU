// Package bot contains Telegram owner-facing onboarding and settings behavior.
package bot

import (
	"fmt"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// DraftEditPromptText returns the message shown to the owner after they click ✏️ Изменить.
func DraftEditPromptText() string {
	return "✏️ Напиши новый текст ответа или опиши правки (например: «сделай короче и без 'вы'»). Следующее твоё сообщение заменит черновик."
}

// DraftFeedbackPromptText returns the message shown after 👎 or reject.
func DraftFeedbackPromptText() string {
	return "👎 Что не так? Напиши пожелание (например: «отвечай менее формально»), или отправь /skip чтобы пропустить. Это поможет боту улучшить ответы."
}

// Layer handles owner-facing bot commands separately from AI message analysis.
type Layer struct {
	repository *store.Repository
}

// NewLayer creates a bot command layer backed by the shared repository.
func NewLayer(repository *store.Repository) *Layer {
	return &Layer{repository: repository}
}

// CommandResponse is the text and audit reason for an owner command.
type CommandResponse struct {
	Text        string
	Reason      string
	ReplyMarkup telegram.ReplyMarkup
}

// HandleCommand applies an owner command and returns owner-facing text.
func (l *Layer) HandleCommand(command telegram.BotCommandUpdate, tenantID string) CommandResponse {
	switch command.Command {
	case "/start":
		if chatID, ok := managedChatID(command.Arguments); ok {
			settings := l.repository.ChatSettings(tenantID, chatID)
			l.repository.SaveChatSettings(settings)
			return CommandResponse{Text: managedChatOnboardingText(chatID, settings), Reason: "managed_chat_start", ReplyMarkup: mainMenuKeyboard()}
		}
		return CommandResponse{Text: startOnboardingText(), Reason: "start_onboarding", ReplyMarkup: mainMenuKeyboard()}
	case "/help":
		return CommandResponse{Text: helpText(), Reason: "help_command", ReplyMarkup: mainMenuKeyboard()}
	case "/settings", "/status":
		return CommandResponse{Text: settingsText(l.repository, tenantID, command.SenderID), Reason: "settings_command", ReplyMarkup: mainMenuKeyboard()}
	case "/chats":
		chats := l.repository.ChatsForTenant(tenantID)
		return CommandResponse{Text: chatsText(l.repository, tenantID), Reason: "chats_command", ReplyMarkup: chatMenuKeyboard(chats, tenantID)}
	case "/mode_manual", "/mode_semi_auto", "/mode_auto", "/mode_silent", "/mode_off":
		return l.handleModeCommand(command, tenantID)
	case "/summary":
		return CommandResponse{Reason: "summary_needs_ai"} // Handled upstream in service.go
	case "/profile":
		return l.handleProfileCommand(command, tenantID)
	case "/profile_set":
		return l.handleProfileSetCommand(command, tenantID)
	default:
		return CommandResponse{Text: helpText(), Reason: "unknown_command"}
	}
}

func (l *Layer) handleProfileCommand(command telegram.BotCommandUpdate, tenantID string) CommandResponse {
	selector := strings.TrimSpace(command.Arguments)
	if selector == "" {
		selector = "1"
	}
	chat, ok := l.repository.ChatBySelector(tenantID, selector)
	if !ok {
		return CommandResponse{Text: "Чат не найден. Используй /chats.", Reason: "profile_chat_not_found", ReplyMarkup: mainMenuKeyboard()}
	}
	profile, _ := l.repository.ContactProfile(tenantID, chat.ChatID)
	score := contextScoreForChat(l.repository, tenantID, chat.ChatID)
	available := l.repository.AvailableModesForChat(tenantID, chat.ChatID)
	nextScore, nextMode := nextUnlockInfo(available)

	lines := []string{
		fmt.Sprintf("👤 Профиль контакта «%s»:", chatName(chat)),
		fmt.Sprintf("• Имя: %s", valueOrDash(profile.Name)),
		fmt.Sprintf("• Пол: %s", valueOrDash(profile.Gender)),
		fmt.Sprintf("• Роль: %s", valueOrDash(profile.Role)),
		fmt.Sprintf("• Тип отношений: %s", valueOrDash(profile.RelationshipType)),
		fmt.Sprintf("• Заметки: %s", valueOrDash(profile.Notes)),
		"",
		fmt.Sprintf("📊 Контекст: %d/100", score),
	}
	if nextMode != "" {
		lines = append(lines, fmt.Sprintf("🔓 До разблокировки %s: %d", nextMode, nextScore-score))
	} else {
		lines = append(lines, "✅ Все режимы разблокированы")
	}
	lines = append(lines, "",
		"Чтобы заполнить профиль, напиши через запятую:",
		fmt.Sprintf("/profile_set %s имя, пол, роль, тип_отношений, заметки", selector),
		"Пример: /profile_set 1 Иван, мужской, клиент, формальные, предпочитает краткие ответы",
	)
	return CommandResponse{Text: strings.Join(lines, "\n"), Reason: "profile_command", ReplyMarkup: mainMenuKeyboard()}
}

func (l *Layer) handleProfileSetCommand(command telegram.BotCommandUpdate, tenantID string) CommandResponse {
	args := strings.TrimSpace(command.Arguments)
	// Format: "<selector> name, gender, role, relationship_type, notes"
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return CommandResponse{Text: "Формат: /profile_set 1 Иван, мужской, клиент, формальные, заметки", Reason: "profile_set_bad_args", ReplyMarkup: mainMenuKeyboard()}
	}
	selector := strings.TrimSpace(parts[0])
	chat, ok := l.repository.ChatBySelector(tenantID, selector)
	if !ok {
		return CommandResponse{Text: "Чат не найден. Используй /chats.", Reason: "profile_set_chat_not_found", ReplyMarkup: mainMenuKeyboard()}
	}
	fields := strings.SplitN(parts[1], ",", 5)
	field := func(i int) string {
		if i < len(fields) {
			return strings.TrimSpace(fields[i])
		}
		return ""
	}
	profile := contracts.ContactProfile{
		TenantID:         tenantID,
		ChatID:           chat.ChatID,
		Name:             field(0),
		Gender:           field(1),
		Role:             field(2),
		RelationshipType: field(3),
		Notes:            field(4),
	}
	l.repository.SaveContactProfile(profile)
	score := contextScoreForChat(l.repository, tenantID, chat.ChatID)
	return CommandResponse{
		Text:        fmt.Sprintf("✅ Профиль «%s» сохранён. Контекст: %d/100", chatName(chat), score),
		Reason:      "profile_set_saved",
		ReplyMarkup: mainMenuKeyboard(),
	}
}

func (l *Layer) handleModeCommand(command telegram.BotCommandUpdate, tenantID string) CommandResponse {
	selector := strings.TrimSpace(command.Arguments)
	if selector == "" {
		return CommandResponse{Text: "Выбери чат из списка: /chats\nЗатем используй команду с номером. Пример: /mode_auto 1", Reason: "mode_command_missing_chat", ReplyMarkup: mainMenuKeyboard()}
	}
	chat, ok := l.repository.ChatBySelector(tenantID, selector)
	if !ok {
		return CommandResponse{Text: "Я пока не вижу такой чат. Открой /chats. Если список пустой, проверь доступ в Telegram Chat Automation или нажми Manage Bot в нужном чате.", Reason: "mode_command_unknown_chat", ReplyMarkup: mainMenuKeyboard()}
	}
	chat = l.repository.ClaimChatForTenant(tenantID, chat)
	mode := modeForCommand(command.Command)

	// Check if the requested mode is unlocked for this chat.
	available := l.repository.AvailableModesForChat(tenantID, chat.ChatID)
	if !modeIsAvailable(mode, available) {
		score := contextScoreForChat(l.repository, tenantID, chat.ChatID)
		needed := scoreNeededForMode(mode)
		return CommandResponse{
			Text:        fmt.Sprintf("🔒 Режим %s ещё не открыт для чата «%s».\n📊 Контекст: %d/%d\n\nЧтобы открыть: заполни профиль контакта /profile %s или продолжи переписку.", mode, chatName(chat), score, needed, selector),
			Reason:      "mode_locked",
			ReplyMarkup: mainMenuKeyboard(),
		}
	}

	current := l.repository.ChatSettings(tenantID, chat.ChatID)
	l.repository.SaveChatSettings(contracts.ChatSettings{
		TenantID:     tenantID,
		ChatID:       chat.ChatID,
		Mode:         mode,
		PrivacyFlags: current.PrivacyFlags,
	})
	return CommandResponse{Text: fmt.Sprintf("Готово. Чат «%s» теперь в режиме %s.", chatName(chat), mode), Reason: "mode_command_updated", ReplyMarkup: chatMenuKeyboard(l.repository.ChatsForTenant(tenantID), tenantID)}
}

// BusinessConnectionText returns owner-facing text for connection status.
func BusinessConnectionText(update telegram.BusinessConnectionUpdate) string {
	if !update.IsEnabled {
		return "Помощник отключен от Telegram Chat Automation. Если хочешь снова включить автоответы, подключи бота заново в настройках Telegram."
	}
	if !update.CanReply {
		return "Бот подключен, но Telegram не дал право отвечать от твоего имени. Ограниченный доступ к выбранным чатам поддерживается, но автоответы будут заблокированы, пока не включишь reply permission в Telegram Settings -> Chat Automation."
	}
	return "Бот подключен к Telegram Chat Automation. Если ты дал доступ только к части чатов, это нормально: они появятся в /chats после первого сообщения или после Manage Bot в нужном чате."
}

func startOnboardingText() string {
	return strings.Join([]string{
		"Привет. Я помощник DoubleU для Telegram Chat Automation.",
		"",
		"Чтобы запустить:",
		"1. Открой Telegram Settings → Chat Automation.",
		"2. Подключи этого бота к профилю.",
		"3. Выбери чаты, к которым даёшь доступ.",
		"4. Если хочешь автоответы, включи право отвечать от твоего имени.",
		"5. После первого сообщения открой /chats и выбери режим.",
		"",
		"Режимы открываются постепенно по мере накопления контекста:",
		"• silent и manual — доступны сразу",
		"• semi_auto — после заполнения профиля контакта (/profile)",
		"• auto — после накопления достаточного контекста",
		"",
		"/help — полный список команд",
	}, "\n")
}

func managedChatOnboardingText(chatID string, settings contracts.ChatSettings) string {
	return fmt.Sprintf("Этот чат подключен к управлению.\nТекущий режим: %s.\n\nЧтобы не вводить chat_id, используй /chats и выбирай чат по номеру.", settings.Mode)
}

func helpText() string {
	return strings.Join([]string{
		"Команды DoubleU:",
		"/start - первичная настройка",
		"/status - статус подключения и режимов",
		"/chats - список чатов и управление режимами",
		"/summary <номер> - сводка выбранного чата",
		"/profile <номер> - профиль контакта и прогресс контекста",
		"/profile_set <номер> имя, пол, роль, отношения, заметки",
		"/mode_silent <номер> - только наблюдение",
		"/mode_manual <номер> - черновики на одобрение",
		"/mode_semi_auto <номер> - умные автоответы",
		"/mode_auto <номер> - полная автономия",
	}, "\n")
}

func settingsText(repository *store.Repository, tenantID, userID string) string {
	connection, hasConnection := repository.BusinessConnectionByUser(userID)
	lines := []string{"Статус DoubleU:"}
	if !hasConnection {
		lines = append(lines, "- Telegram Chat Automation еще не подключен.")
	} else {
		lines = append(lines,
			fmt.Sprintf("- Business connection: %s", connection.ConnectionID),
			fmt.Sprintf("- enabled: %t", connection.IsEnabled),
			fmt.Sprintf("- can_reply: %t", connection.CanReply),
		)
		if !connection.CanReply {
			lines = append(lines, "- Ограниченный доступ принят. Автоответы будут заблокированы, пока Telegram не даст can_reply=true.")
		}
	}
	lines = append(lines, "", chatsText(repository, tenantID))
	return strings.Join(lines, "\n")
}

func chatsText(repository *store.Repository, tenantID string) string {
	chats := repository.ChatsForTenant(tenantID)
	if len(chats) == 0 {
		return strings.Join([]string{
			"Доступные чаты пока не найдены.",
			"Если ты дал ограниченный доступ, это нормально: Telegram пришлет чат после первого входящего сообщения или после Manage Bot в этом чате.",
			"Проверь, что чат выбран в Telegram Settings -> Chat Automation.",
		}, "\n")
	}
	lines := []string{"Доступные чаты:"}
	for index, chat := range chats {
		settings := repository.ChatSettings(tenantID, chat.ChatID)
		lines = append(lines, fmt.Sprintf("%d. %s — режим %s", index+1, chatName(chat), settings.Mode))
	}
	lines = append(lines, "", "Пример: /mode_auto 1 или /mode_manual 2")
	return strings.Join(lines, "\n")
}

func managedChatID(arguments string) (string, bool) {
	argument := strings.TrimSpace(arguments)
	if strings.HasPrefix(argument, "bizChat") && len(argument) > len("bizChat") {
		return strings.TrimPrefix(argument, "bizChat"), true
	}
	return "", false
}

func modeForCommand(command string) contracts.ChatMode {
	switch command {
	case "/mode_auto":
		return contracts.ChatModeAuto
	case "/mode_semi_auto":
		return contracts.ChatModeSemiAuto
	case "/mode_silent":
		return contracts.ChatModeSilent
	case "/mode_off":
		return contracts.ChatModeDisabled
	default:
		return contracts.ChatModeManual
	}
}

func chatName(chat store.ChatRecord) string {
	if chat.DisplayName != "" {
		return chat.DisplayName
	}
	return "chat " + chat.ChatID
}

func modeIsAvailable(mode contracts.ChatMode, available []contracts.ChatMode) bool {
	for _, m := range available {
		if m == mode {
			return true
		}
	}
	return false
}

func contextScoreForChat(repository *store.Repository, tenantID, chatID string) int {
	return repository.ContextScoreForChat(tenantID, chatID)
}

func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func nextUnlockInfo(available []contracts.ChatMode) (int, contracts.ChatMode) {
	hasSemi, hasAuto := false, false
	for _, m := range available {
		if m == contracts.ChatModeSemiAuto {
			hasSemi = true
		}
		if m == contracts.ChatModeAuto {
			hasAuto = true
		}
	}
	if !hasSemi {
		return 50, contracts.ChatModeSemiAuto
	}
	if !hasAuto {
		return 80, contracts.ChatModeAuto
	}
	return 0, ""
}

func scoreNeededForMode(mode contracts.ChatMode) int {
	switch mode {
	case contracts.ChatModeSemiAuto:
		return 50
	case contracts.ChatModeAuto:
		return 80
	default:
		return 0
	}
}

// DraftCardText formats the draft notification sent to the owner.
func DraftCardText(contactMsg, draftText, chatName string) string {
	lines := []string{
		fmt.Sprintf("💬 Новое сообщение от %s:", chatName),
		"",
		contactMsg,
		"",
		"📝 Черновик ответа:",
		"",
		draftText,
	}
	return strings.Join(lines, "\n")
}

// draftActionRows returns the core action rows. noMemory reflects the current chat memory state.
func draftActionRows(draftID string, noMemory bool) [][]telegram.InlineKeyboardButton {
	enc := contracts.CallbackDataEncode
	memoryBtn := telegram.InlineKeyboardButton{
		Text:         "🟢 Память вкл",
		CallbackData: enc(contracts.ApprovalNoMemory, draftID),
	}
	if noMemory {
		memoryBtn.Text = "🔴 Память выкл"
	}
	return [][]telegram.InlineKeyboardButton{
		{
			{Text: "✅ Отправить", CallbackData: enc(contracts.ApprovalApprove, draftID)},
			{Text: "❌ Отклонить", CallbackData: enc(contracts.ApprovalReject, draftID)},
		},
		{
			{Text: "🔄 Переписать", CallbackData: enc(contracts.ApprovalRegenerate, draftID)},
			{Text: "🤖 Уточнить", CallbackData: enc(contracts.ApprovalAIRefine, draftID)},
		},
		{
			{Text: "✏️ Заменить текст", CallbackData: enc(contracts.ApprovalEditRequest, draftID)},
			memoryBtn,
		},
	}
}

// DraftApprovalKeyboardNoRating returns the approval keyboard without the 👍👎 row (used after rating).
func DraftApprovalKeyboardNoRating(draftID string, noMemory bool) telegram.InlineKeyboardMarkup {
	return telegram.InlineKeyboardMarkup{InlineKeyboard: draftActionRows(draftID, noMemory)}
}

// DraftApprovalKeyboard returns the inline keyboard attached to a draft card.
func DraftApprovalKeyboard(draftID string, noMemory bool) telegram.InlineKeyboardMarkup {
	enc := contracts.CallbackDataEncode
	rows := draftActionRows(draftID, noMemory)
	rows = append(rows, []telegram.InlineKeyboardButton{
		{Text: "👍", CallbackData: enc(contracts.ApprovalThumbUp, draftID)},
		{Text: "👎", CallbackData: enc(contracts.ApprovalThumbDown, draftID)},
	})
	return telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// NoMemoryStatusText returns the confirmation message after toggling the no_memory flag.
func NoMemoryStatusText(enabled bool) string {
	if enabled {
		return "🚫 Память отключена для этого чата. ИИ не будет запоминать контекст.\nНажми «Без памяти» снова чтобы включить."
	}
	return "✅ Память включена. ИИ снова запоминает контекст переписки."
}

// AIRefinePromptText returns the message shown when owner clicks 🤖 Уточнить.
func AIRefinePromptText() string {
	return "🤖 Напиши инструкцию для ИИ — что изменить в черновике.\nПримеры: «сделай короче», «убери формальность», «добавь про доставку», «поменяй тон на дружеский»."
}

func mainMenuKeyboard() telegram.ReplyKeyboardMarkup {
	return telegram.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard: [][]telegram.KeyboardButton{
			{{Text: "/chats"}, {Text: "/settings"}},
			{{Text: "/help"}},
		},
	}
}

func chatMenuKeyboard(chats []store.ChatRecord, tenantID string) telegram.ReplyKeyboardMarkup {
	if len(chats) == 0 {
		return mainMenuKeyboard()
	}
	rows := [][]telegram.KeyboardButton{
		{{Text: "/chats"}, {Text: "/settings"}},
	}
	for index := range chats {
		if index >= 3 {
			break
		}
		number := fmt.Sprintf("%d", index+1)
		rows = append(rows,
			[]telegram.KeyboardButton{{Text: "/mode_silent " + number}, {Text: "/mode_manual " + number}},
			[]telegram.KeyboardButton{{Text: "/mode_semi_auto " + number}, {Text: "/mode_auto " + number}},
		)
	}
	// tenantID reserved for future per-chat unlock display.
	_ = tenantID
	return telegram.ReplyKeyboardMarkup{ResizeKeyboard: true, Keyboard: rows}
}

// QuestionCardText formats the owner notification for an incoming choice question.
// Unlike DraftCardText it does not include a pre-written reply — the owner picks from options.
func QuestionCardText(contactMsg, chatName string) string {
	lines := []string{
		fmt.Sprintf("❓ Вас спрашивают (%s):", chatName),
		"",
		contactMsg,
		"",
		"👇 Выберите ответ или напишите свой:",
	}
	return strings.Join(lines, "\n")
}

// QuestionOptionsKeyboard builds an inline keyboard with one button per option plus a
// "Напишу сам" button that activates the owner text-edit flow.
// Up to 9 options are supported (single-digit index in callback_data).
func QuestionOptionsKeyboard(draftID string, options []string) telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, len(options)+1)
	for i, opt := range options {
		if i > 8 {
			break // enforce single-digit index limit
		}
		label := opt
		if len(label) > 30 {
			label = label[:27] + "..."
		}
		rows = append(rows, []telegram.InlineKeyboardButton{
			{Text: label, CallbackData: contracts.CallbackDataEncodeChoice(draftID, i)},
		})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{
		{Text: "✏️ Напишу сам", CallbackData: contracts.CallbackDataEncode(contracts.ApprovalEditRequest, draftID)},
	})
	return telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}
