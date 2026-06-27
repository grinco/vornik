package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeCallback_HappyPath(t *testing.T) {
	out, err := EncodeCallback("project", "select", "my-project")
	require.NoError(t, err)
	assert.Equal(t, "project:select:my-project", out)
}

func TestEncodeCallback_RejectsDelimiterInNamespaceOrAction(t *testing.T) {
	_, err := EncodeCallback("ns:bad", "select", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain")

	_, err = EncodeCallback("project", "bad:action", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain")
}

func TestEncodeCallback_RejectsOversizedPayload(t *testing.T) {
	// 64-byte cap leaves ~50 chars for the payload after namespace
	// + action + 2 delimiters. Send a 64-char payload to push the
	// total over.
	huge := strings.Repeat("a", 64)
	_, err := EncodeCallback("project", "select", huge)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Telegram caps callback_data at 64")
}

func TestDecodeCallback_HappyPath(t *testing.T) {
	ns, action, payload, ok := DecodeCallback("project:select:my-project")
	require.True(t, ok)
	assert.Equal(t, "project", ns)
	assert.Equal(t, "select", action)
	assert.Equal(t, "my-project", payload)
}

func TestDecodeCallback_PreservesDelimiterInPayload(t *testing.T) {
	// A future namespace might want hierarchical payloads like
	// "trade:approve:order-id:revision-3". The decoder must split
	// on the FIRST two delimiters only.
	ns, action, payload, ok := DecodeCallback("trade:approve:order-id:rev-3")
	require.True(t, ok)
	assert.Equal(t, "trade", ns)
	assert.Equal(t, "approve", action)
	assert.Equal(t, "order-id:rev-3", payload,
		"payload must keep its own delimiters — only the first two are structural")
}

func TestDecodeCallback_RejectsMalformed(t *testing.T) {
	for _, data := range []string{
		"",              // empty
		"no-delimiter",  // no colons at all
		"only-one:part", // only one colon
		":missing-ns:x", // empty namespace
		"ns::missing",   // empty action
	} {
		t.Run(data, func(t *testing.T) {
			_, _, _, ok := DecodeCallback(data)
			assert.False(t, ok, "malformed callback %q must NOT decode — the dispatcher swallows it as 'stale button'", data)
		})
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	cases := []struct {
		ns, action, payload string
	}{
		{"project", "select", "asst"},
		{"trade", "approve", "order_123_abc"},
		{"memory", "save", "chunk_42"},
		{"project", "menu", "status"},
	}
	for _, tc := range cases {
		t.Run(tc.ns+":"+tc.action, func(t *testing.T) {
			encoded, err := EncodeCallback(tc.ns, tc.action, tc.payload)
			require.NoError(t, err)
			gotNs, gotAction, gotPayload, ok := DecodeCallback(encoded)
			require.True(t, ok)
			assert.Equal(t, tc.ns, gotNs)
			assert.Equal(t, tc.action, gotAction)
			assert.Equal(t, tc.payload, gotPayload)
		})
	}
}

func TestKeyboardOneCol_StacksOnePerRow(t *testing.T) {
	kb := KeyboardOneCol(
		Button{Text: "Alpha", Data: "ns:a:x"},
		Button{Text: "Beta", Data: "ns:b:y"},
		Button{Text: "Gamma", Data: "ns:c:z"},
	)
	require.Len(t, kb.InlineKeyboard, 3, "one-col must put each button on its own row")
	for i, row := range kb.InlineKeyboard {
		require.Len(t, row, 1, "row %d should have exactly one button", i)
	}
	assert.Equal(t, "Alpha", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "ns:a:x", kb.InlineKeyboard[0][0].CallbackData)
}

func TestKeyboardGrid_RowMajor(t *testing.T) {
	kb := KeyboardGrid(2,
		Button{Text: "A", Data: "ns:a:1"},
		Button{Text: "B", Data: "ns:a:2"},
		Button{Text: "C", Data: "ns:a:3"},
		Button{Text: "D", Data: "ns:a:4"},
		Button{Text: "E", Data: "ns:a:5"},
	)
	require.Len(t, kb.InlineKeyboard, 3, "5 buttons in 2-col grid → 3 rows (2+2+1)")
	require.Len(t, kb.InlineKeyboard[0], 2)
	require.Len(t, kb.InlineKeyboard[1], 2)
	require.Len(t, kb.InlineKeyboard[2], 1, "trailing partial row keeps remaining buttons")
	assert.Equal(t, "E", kb.InlineKeyboard[2][0].Text)
}

func TestKeyboardGrid_ZeroColsDegradesToOneCol(t *testing.T) {
	kb := KeyboardGrid(0,
		Button{Text: "X", Data: "ns:a:1"},
		Button{Text: "Y", Data: "ns:a:2"},
	)
	require.Len(t, kb.InlineKeyboard, 2, "cols ≤ 0 degrades to one-col so the keyboard still renders")
}

func TestKeyboardOneCol_PanicsOnOversizedCallback(t *testing.T) {
	huge := Button{Text: "X", Data: strings.Repeat("a", 65)}
	defer func() {
		r := recover()
		require.NotNil(t, r, "constructor must panic on oversized callback_data — silent strip-at-delivery is a much worse failure mode")
		assert.Contains(t, r.(string), "exceeds Telegram cap")
	}()
	_ = KeyboardOneCol(huge)
}
