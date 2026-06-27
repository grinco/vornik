package conversation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeChannelSpecific_NilAndEmpty(t *testing.T) {
	if got := SanitizeChannelSpecific(nil); got != nil {
		t.Errorf("nil input = %v, want nil", got)
	}
	if got := SanitizeChannelSpecific(map[string]string{}); got != nil {
		t.Errorf("empty input = %v, want nil", got)
	}
}

func TestSanitizeChannelSpecific_StripsControlChars(t *testing.T) {
	in := map[string]string{
		"team_id":   "T123\n\r\x00injected",
		"thread\tk": "ok",
	}
	out := SanitizeChannelSpecific(in)
	if out["team_id"] != "T123injected" {
		t.Errorf("value not stripped of control bytes: %q", out["team_id"])
	}
	// The key's tab is stripped → "threadk".
	if _, ok := out["threadk"]; !ok {
		t.Errorf("key not stripped of control bytes: %v", out)
	}
}

func TestSanitizeChannelSpecific_BoundsEntriesAndLengths(t *testing.T) {
	in := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		in["k"+string(rune('a'+i%26))+itoa(i)] = "v"
	}
	in["bigval"] = strings.Repeat("x", maxChannelSpecificValLen+500)
	in[strings.Repeat("K", maxChannelSpecificKeyLen+50)] = "v"

	out := SanitizeChannelSpecific(in)
	if len(out) > maxChannelSpecificEntries {
		t.Errorf("kept %d entries, want <= %d", len(out), maxChannelSpecificEntries)
	}
	for k, v := range out {
		if utf8.RuneCountInString(k) > maxChannelSpecificKeyLen {
			t.Errorf("key over cap: %d runes", utf8.RuneCountInString(k))
		}
		if utf8.RuneCountInString(v) > maxChannelSpecificValLen {
			t.Errorf("value over cap: %d runes", utf8.RuneCountInString(v))
		}
	}
}

func TestSanitizeChannelSpecific_PreservesUTF8(t *testing.T) {
	in := map[string]string{"name": "Vadim Grinço 日本語"}
	out := SanitizeChannelSpecific(in)
	if out["name"] != "Vadim Grinço 日本語" {
		t.Errorf("multibyte UTF-8 mangled: %q", out["name"])
	}
}

func TestSanitizeChannelSpecific_DropsAllControlKey(t *testing.T) {
	out := SanitizeChannelSpecific(map[string]string{"\n\t\x00": "v"})
	if out != nil {
		t.Errorf("all-control key should drop to nil map, got %v", out)
	}
}

// itoa avoids importing strconv just for the test fixture.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
