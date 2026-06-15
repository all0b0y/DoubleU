package contracts

import "strings"

// botChatPrefix marks a synthetic chat id that is scoped to a specific bot.
// Bot-transport chats are namespaced as "bot:{botID}:{telegramChatID}" so that
// two of a tenant's bots talking to the SAME Telegram user resolve to two
// distinct chats (separate memory, drafts, role). Personal/business transport
// keeps the raw Telegram chat id with no namespacing.
const botChatPrefix = "bot:"

// BotChatID builds the bot-scoped synthetic chat id for a bot-transport chat.
// The raw Telegram chat id is recoverable via RawChatID / ParseBotChatID, and
// is also persisted in chats.telegram_chat_id for outbound sends.
func BotChatID(botID, telegramChatID string) string {
	return botChatPrefix + botID + ":" + telegramChatID
}

// ParseBotChatID splits a bot-scoped chat id back into its bot id and raw
// Telegram chat id. ok is false for ids that are not bot-scoped (e.g. business
// transport), in which case callers should treat the id as a raw Telegram id.
func ParseBotChatID(chatID string) (botID, telegramChatID string, ok bool) {
	if !strings.HasPrefix(chatID, botChatPrefix) {
		return "", "", false
	}
	rest := chatID[len(botChatPrefix):]
	// SplitN on the FIRST ":" only — a Telegram chat id never contains ":".
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// RawChatID returns the Telegram chat id to address when sending: the embedded
// raw id for bot-scoped chats, or the id unchanged for personal/business chats.
func RawChatID(chatID string) string {
	if _, raw, ok := ParseBotChatID(chatID); ok {
		return raw
	}
	return chatID
}
