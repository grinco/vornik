package service

import "testing"

// TestTelegramPollerBotID pins the bot-id derivation. The
// derived id keys the telegram_poller_state row so it MUST
// be stable per bot identity and MUST NOT contain the secret
// half of the token (operators inspecting the DB shouldn't
// see the password).
func TestTelegramPollerBotID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty token disables persistence", "", ""},
		{"colon-form yields numeric prefix", "123456:ABCDEF-secret-half", "tg:123456"},
		{"missing colon falls back to length-bounded prefix", "noColonHereJustText", "tg:noColonH"},
		{"short missing-colon token preserves all chars", "short", "tg:short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramPollerBotID(tc.in)
			if got != tc.want {
				t.Errorf("telegramPollerBotID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Defence-in-depth: a real-shaped token must never expose
	// the secret half. Cover the canonical Telegram form.
	const real = "5912345678:AAH-secretAuthorizationStringThatMustNotLeak"
	id := telegramPollerBotID(real)
	if id == "" {
		t.Errorf("real-shaped token should yield a non-empty id")
	}
	if got := id; len(got) > 0 && got[len(got)-1] != '8' /* last digit before colon */ {
		// Loose guard: the id must end with a digit (the bot's
		// numeric id), not a letter from the secret half.
		t.Errorf("derived id %q appears to leak the secret half", got)
	}
}
