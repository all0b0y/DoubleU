package core

import "testing"

// TestRedactSecrets verifies the ingestion redactor's contract: credential/PII
// values are masked with a category placeholder while ordinary text (including
// non-card long numbers) is left untouched.
func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		redacted bool
	}{
		{"visa card masked", "оплати картой 4111 1111 1111 1111 сегодня", "оплати картой [номер карты] сегодня", true},
		{"password with value masked", "мой пароль: qwerty123", "мой [пароль]", true},
		{"cvv masked", "cvv 123", "[cvv]", true},
		{"otp code masked", "код подтверждения 456789", "[код]", true},
		{"pin masked", "пин-код 4321", "[пин-код]", true},
		{"plain conversation untouched", "давай встретимся завтра в 10", "давай встретимся завтра в 10", false},
		{"short number sequence untouched", "мой телефон 8 900 123 45 67", "мой телефон 8 900 123 45 67", false},
		{"password word without value untouched", "я забыл пароль от почты", "я забыл пароль от почты", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, redacted := redactSecrets(tc.in)
			if got != tc.want {
				t.Fatalf("redactSecrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if redacted != tc.redacted {
				t.Fatalf("redactSecrets(%q) redacted=%v, want %v", tc.in, redacted, tc.redacted)
			}
		})
	}
}

// TestRedactSecretsIdempotent guards the double-redaction path (claimed vs
// passive ingestion): masking already-masked text must be a no-op.
func TestRedactSecretsIdempotent(t *testing.T) {
	once, _ := redactSecrets("пароль: hunter2 и карта 4111 1111 1111 1111")
	twice, redacted := redactSecrets(once)
	if twice != once {
		t.Fatalf("second redaction changed text: %q -> %q", once, twice)
	}
	if redacted {
		t.Fatalf("second redaction reported a change on already-masked text")
	}
}
