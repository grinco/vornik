// Package ui: focused option-wire tests for the chat handler's
// surface. These are tiny helper paths but they're load-bearing —
// without WithChatDispatcher the chat page renders the disabled
// banner; without WithWebUIBaseURL deliverable links fall back to
// relative paths. Each option lands in a Server field, so we assert
// the field after construction.
package ui

import (
	"testing"
)

func TestWithChatDispatcher_SetsField(t *testing.T) {
	// Construct with a non-nil sentinel (reusing the test stub from
	// chat_test.go) and confirm the field landed on the Server.
	stub := &stubChatDispatcher{reply: "hi"}
	srv := NewServer(WithChatDispatcher(stub))
	if srv.chatDispatcher == nil {
		t.Fatal("WithChatDispatcher did not set chatDispatcher")
	}
	if srv.chatDispatcher != stub {
		t.Errorf("chatDispatcher: got %T, want *stubChatDispatcher", srv.chatDispatcher)
	}
}

func TestWithChatDispatcher_NilAllowed(t *testing.T) {
	// Documented: nil is the "disabled" form. NewServer must accept
	// it without panic; the chat handler then renders the disabled
	// banner.
	srv := NewServer(WithChatDispatcher(nil))
	if srv.chatDispatcher != nil {
		t.Errorf("expected nil chatDispatcher, got %T", srv.chatDispatcher)
	}
}

func TestWithWebUIBaseURL_StripsTrailingSlash(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://vornik.example.com", "https://vornik.example.com"},
		{"https://vornik.example.com/", "https://vornik.example.com"},
		{"https://vornik.example.com////", "https://vornik.example.com"},
		{"", ""},
		{"/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			srv := NewServer(WithWebUIBaseURL(tc.in))
			if srv.webUIBaseURL != tc.want {
				t.Errorf("got %q, want %q", srv.webUIBaseURL, tc.want)
			}
		})
	}
}
