package cli

// Coverage sweep for `vornikctl operator {link,unlink,show-links}`.
// These handlers write to cmd.OutOrStdout(), so the tests pass a cobra
// command with a bytes.Buffer set as its out — no stdout capture needed.
// httptest harness for the daemon side.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func operatorLinksCov_cmd() (*cobra.Command, *bytes.Buffer) {
	c := &cobra.Command{}
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	return c, buf
}

func operatorLinksCov_reset() {
	operatorLinkLinkedBy = "cli"
	operatorLinkJSON, operatorShowLinksJSON = false, false
}

func TestRunOperatorLink_ValidationBranches(t *testing.T) {
	operatorLinksCov_reset()
	c, _ := operatorLinksCov_cmd()
	if err := runOperatorLink(c, []string{"", "x"}); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("expected non-empty error, got %v", err)
	}
	if err := runOperatorLink(c, []string{"same", "same"}); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected self-link error, got %v", err)
	}
}

func TestRunOperatorLink_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/operators/webchat:abc/links" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["channel_speaker_id"] != "telegram:42" {
			t.Errorf("speaker not forwarded: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(operatorLinkEntry{
			ChannelSpeakerID: "telegram:42", OperatorID: "webchat:abc", LinkedBy: "cli",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	c, buf := operatorLinksCov_cmd()
	if err := runOperatorLink(c, []string{"webchat:abc", "telegram:42"}); err != nil {
		t.Fatalf("runOperatorLink: %v", err)
	}
	if !strings.Contains(buf.String(), "Linked telegram:42 → webchat:abc") {
		t.Errorf("link output: %s", buf.String())
	}
}

func TestRunOperatorLink_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(operatorLinkEntry{ChannelSpeakerID: "s", OperatorID: "o"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	operatorLinkJSON = true
	c, buf := operatorLinksCov_cmd()
	if err := runOperatorLink(c, []string{"o", "s"}); err != nil {
		t.Fatalf("runOperatorLink json: %v", err)
	}
	var decoded operatorLinkEntry
	if jerr := json.Unmarshal(buf.Bytes(), &decoded); jerr != nil || decoded.OperatorID != "o" {
		t.Fatalf("bad JSON output: %v %s", jerr, buf.String())
	}
}

func TestRunOperatorLink_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "already linked"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	c, _ := operatorLinksCov_cmd()
	if err := runOperatorLink(c, []string{"o", "s"}); err == nil {
		t.Fatal("expected error on 409")
	}
}

func TestRunOperatorUnlink_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/links/telegram:42") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	c, buf := operatorLinksCov_cmd()
	if err := runOperatorUnlink(c, []string{"telegram:42"}); err != nil {
		t.Fatalf("runOperatorUnlink: %v", err)
	}
	if !strings.Contains(buf.String(), "Unlinked telegram:42") {
		t.Errorf("unlink output: %s", buf.String())
	}
}

func TestRunOperatorUnlink_EmptyIsError(t *testing.T) {
	operatorLinksCov_reset()
	c, _ := operatorLinksCov_cmd()
	if err := runOperatorUnlink(c, []string{"  "}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required error, got %v", err)
	}
}

func TestRunOperatorShowLinks_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/operators/op-1/links" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(operatorLinksResponse{Links: []operatorLinkEntry{
			{ChannelSpeakerID: "telegram:1", OperatorID: "op-1", LinkedAt: "2026-06-02", LinkedBy: "cli"},
			{ChannelSpeakerID: "slack:9", OperatorID: "op-1", LinkedAt: "2026-06-01", LinkedBy: "self"},
		}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	c, buf := operatorLinksCov_cmd()
	out, err := captureStdoutFunc(t, func() error { return runOperatorShowLinks(c, []string{"op-1"}) })
	if err != nil {
		t.Fatalf("runOperatorShowLinks: %v", err)
	}
	// The table goes to os.Stdout (tabwriter), the trailing total to cmd out.
	combined := out + buf.String()
	for _, want := range []string{"telegram:1", "slack:9", "Total: 2 link"} {
		if !strings.Contains(combined, want) {
			t.Errorf("show-links output missing %q in:\n%s", want, combined)
		}
	}
	// slack:9 linked earlier → sorts before telegram:1.
	if strings.Index(out, "slack:9") > strings.Index(out, "telegram:1") {
		t.Errorf("links not sorted by linked_at:\n%s", out)
	}
}

func TestRunOperatorShowLinks_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(operatorLinksResponse{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	operatorLinksCov_reset()
	c, buf := operatorLinksCov_cmd()
	if err := runOperatorShowLinks(c, []string{"op-empty"}); err != nil {
		t.Fatalf("runOperatorShowLinks empty: %v", err)
	}
	if !strings.Contains(buf.String(), "No links pointing at op-empty") {
		t.Errorf("empty output: %s", buf.String())
	}
}
