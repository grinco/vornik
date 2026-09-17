package telegram

import (
	"testing"
)

func TestTelegramSpeakerID(t *testing.T) {
	// The argument is a USER id (2026-09-17). The negative case stays because
	// the formatter is total and must not be surprising, NOT because a
	// supergroup id is a legitimate input — a negative value here would mean
	// a caller has gone back to passing the chat id, which is the defect that
	// made group links inert. TestLink_OTPIsIssuedUnderTheUserIDNotTheChatID
	// is what guards the caller.
	cases := map[int64]string{
		42:         "telegram:42",
		-100123456: "telegram:-100123456",
		0:          "telegram:0",
	}
	for in, want := range cases {
		if got := telegramSpeakerID(in); got != want {
			t.Errorf("telegramSpeakerID(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinHumanList(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b, and c"},
		{[]string{"a", "b", "c", "d"}, "a, b, c, and d"},
	}
	for _, tc := range cases {
		if got := joinHumanList(tc.in); got != tc.want {
			t.Errorf("joinHumanList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
