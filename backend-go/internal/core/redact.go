package core

import (
	"regexp"
	"strings"
)

// PII / credential redaction at ingestion.
//
// Sensitive values a contact (or the owner) pastes into a chat — passwords,
// payment-card numbers, CVV, one-time codes — must never be persisted raw or
// echoed by the model, so they are masked with a category placeholder BEFORE the
// message is stored, sent to the AI, extracted into memory, or embedded (owner
// policy, 2026-07-04: "не должно попасть в интернет"). The mask is general
// (pattern + light validation), not tuned to any single message, and because it
// runs before the AI turn the model physically cannot echo the secret.
var (
	// A run of 13–19 digits, optionally grouped by single spaces or dashes
	// (card-like). Luhn-validated in redactSecrets so ordinary long numbers
	// (order ids, phone numbers) are left intact.
	reCardNumber = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)

	// Keyword-anchored secrets: the label plus its following value. Go's RE2 `\b`
	// is ASCII-only (Cyrillic isn't `\w`), so the left edge is a captured Unicode
	// non-letter boundary (or start) preserved via ${1} in the replacement; the
	// whole match otherwise collapses to a labelled placeholder.
	reCVV      = regexp.MustCompile(`(?i)(^|[^\p{L}])(?:cvv2?|cvc)[\s:=«"'-]*\d{3,4}`)
	rePIN      = regexp.MustCompile(`(?i)(^|[^\p{L}])(?:пин-?код|pin-?code|pin)[\s:=«"'-]*\d{3,6}`)
	reCode     = regexp.MustCompile(`(?i)(^|[^\p{L}])(?:код подтверждения|смс-?код|одноразовый код|verification code|otp|2fa)[\s:=«"'-]*\d{3,8}`)
	rePassword = regexp.MustCompile(`(?i)(^|[^\p{L}])(?:пароль|пасворд|password|passcode)[\s:=«"'-]*\S{4,}`)
)

// redactSecrets masks credential/PII spans with category placeholders, returning
// the redacted text and whether anything was masked. Labelled secrets are masked
// before card numbers, since a labelled value may itself contain a digit run.
func redactSecrets(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	original := text
	text = reCVV.ReplaceAllString(text, "${1}[cvv]")
	text = rePIN.ReplaceAllString(text, "${1}[пин-код]")
	text = reCode.ReplaceAllString(text, "${1}[код]")
	text = rePassword.ReplaceAllString(text, "${1}[пароль]")
	text = reCardNumber.ReplaceAllStringFunc(text, func(m string) string {
		digits := digitsOnly(m)
		if len(digits) >= 13 && len(digits) <= 19 && luhnValid(digits) {
			return "[номер карты]"
		}
		return m
	})
	return text, text != original
}

// digitsOnly returns the ASCII digits of s with every other rune removed.
func digitsOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// luhnValid reports whether the digit string passes the Luhn checksum used by
// payment cards — a cheap guard so ordinary 13–19 digit numbers are not masked.
func luhnValid(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
