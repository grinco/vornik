package telegram

import (
	"fmt"
	"strings"
)

// Inline-keyboard primitives — 2026.6.0 SaaS-readiness feature 3.
// Wraps Telegram's InlineKeyboardMarkup with a tiny vocabulary
// (Button, KeyboardOneCol, KeyboardGrid) and a callback-data
// encoder so handlers can stay declarative.
//
// Callback data is capped at 64 bytes per Telegram's API; the
// encoder splits a (namespace, action, payload) tuple into a
// colon-delimited string and refuses anything too long up front
// so a buggy handler can't produce buttons Telegram silently
// drops on click. Long IDs (e.g. vornik task IDs are 47 chars
// including prefix) need short aliases — the per-chat session
// store handles that round-trip.

// inlineButtonMaxBytes is the Telegram API cap on
// InlineKeyboardButton.callback_data length. Documented at
// https://core.telegram.org/bots/api#inlinekeyboardbutton.
const inlineButtonMaxBytes = 64

// callbackDelimiter separates the three segments of an inline-
// keyboard callback payload (namespace:action:payload). Chosen
// because it never appears in any of vornik's existing project /
// task / role identifiers — which use lowercase, digits, and
// hyphens — so encode/decode round-trips cleanly without escaping.
const callbackDelimiter = ":"

// Button is one cell in an inline keyboard. Text is what the user
// sees; Data is the opaque payload Telegram returns on click.
type Button struct {
	Text string
	Data string
}

// InlineKeyboardMarkup is the JSON shape Telegram expects for
// reply_markup. One row per outer slice; one button per inner.
// Mirrors tgbotapi's struct but lives here so the bot package
// doesn't gain a tgbotapi dependency just for this slice.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

// inlineKeyboardButton matches the Telegram Bot API JSON exactly:
// `text` is the operator-visible label, `callback_data` rides the
// button click back to the bot via the next CallbackQuery update.
// Unexported because callers always build it through the
// KeyboardOneCol / KeyboardGrid helpers below — those enforce the
// length cap on callback_data.
type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// KeyboardOneCol renders the given buttons stacked vertically
// (one per row). Useful when each button has a long label and
// horizontal layout would wrap awkwardly on mobile.
//
// Panics on a button whose callback data exceeds the Telegram
// 64-byte cap — caller bugs in keyboard construction should fail
// loudly in dev rather than producing buttons that Telegram
// silently strips at delivery time.
func KeyboardOneCol(buttons ...Button) InlineKeyboardMarkup {
	rows := make([][]inlineKeyboardButton, 0, len(buttons))
	for _, b := range buttons {
		if len(b.Data) > inlineButtonMaxBytes {
			panic(fmt.Sprintf("inline button %q: callback_data %d bytes exceeds Telegram cap %d — use a per-session alias", b.Text, len(b.Data), inlineButtonMaxBytes))
		}
		rows = append(rows, []inlineKeyboardButton{{Text: b.Text, CallbackData: b.Data}})
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

// KeyboardGrid renders buttons in a grid `cols` wide, row-major.
// Useful for project pickers, menu actions, and any keyboard with
// short labels where space matters more than line-by-line clarity.
//
// `cols` ≤ 0 is treated as 1 (degrades to one-column rather than
// crashing). Trailing partial rows are emitted with however many
// buttons remain.
func KeyboardGrid(cols int, buttons ...Button) InlineKeyboardMarkup {
	if cols <= 0 {
		cols = 1
	}
	rows := make([][]inlineKeyboardButton, 0, (len(buttons)+cols-1)/cols)
	current := make([]inlineKeyboardButton, 0, cols)
	for _, b := range buttons {
		if len(b.Data) > inlineButtonMaxBytes {
			panic(fmt.Sprintf("inline button %q: callback_data %d bytes exceeds Telegram cap %d — use a per-session alias", b.Text, len(b.Data), inlineButtonMaxBytes))
		}
		current = append(current, inlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		if len(current) == cols {
			rows = append(rows, current)
			current = make([]inlineKeyboardButton, 0, cols)
		}
	}
	if len(current) > 0 {
		rows = append(rows, current)
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

// EncodeCallback formats a (namespace, action, payload) tuple into
// a callback_data string. Stable across versions — adding a new
// namespace doesn't change the wire format. Returns an error when
// the encoded result would exceed Telegram's 64-byte cap so the
// caller can route the payload through a per-session alias before
// rendering the keyboard.
//
// `ns` and `action` MUST NOT contain the delimiter; an embedded
// colon would corrupt the round-trip through DecodeCallback. We
// reject at encode time rather than silently quoting because the
// vocabulary is small and operator-controlled.
func EncodeCallback(ns, action, payload string) (string, error) {
	if strings.Contains(ns, callbackDelimiter) {
		return "", fmt.Errorf("callback namespace %q must not contain %q", ns, callbackDelimiter)
	}
	if strings.Contains(action, callbackDelimiter) {
		return "", fmt.Errorf("callback action %q must not contain %q", action, callbackDelimiter)
	}
	out := ns + callbackDelimiter + action + callbackDelimiter + payload
	if len(out) > inlineButtonMaxBytes {
		return "", fmt.Errorf("encoded callback %q is %d bytes; Telegram caps callback_data at %d (use a per-session alias for long payloads)", out, len(out), inlineButtonMaxBytes)
	}
	return out, nil
}

// DecodeCallback parses a callback_data string back into its
// (namespace, action, payload) tuple. Returns ok=false on any
// malformed input — the bot's callback dispatcher treats this as
// "ignore the click" rather than 500-ing, because the payload
// could come from a stale browser tab or a deliberately-malformed
// client and shouldn't crash the bot.
//
// Splits on the FIRST two delimiters only — payload itself may
// contain delimiters (URL-safe IDs, future namespacing) without
// confusing the parser.
func DecodeCallback(data string) (ns, action, payload string, ok bool) {
	first := strings.Index(data, callbackDelimiter)
	if first < 0 {
		return "", "", "", false
	}
	rest := data[first+1:]
	second := strings.Index(rest, callbackDelimiter)
	if second < 0 {
		return "", "", "", false
	}
	ns = data[:first]
	action = rest[:second]
	payload = rest[second+1:]
	if ns == "" || action == "" {
		return "", "", "", false
	}
	return ns, action, payload, true
}
