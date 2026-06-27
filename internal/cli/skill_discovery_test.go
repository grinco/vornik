package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSkillSearch_Tabular(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	var out bytes.Buffer
	skillSearchCmd.SetOut(&out)
	skillSearchCmd.SetErr(&out)
	skillSearchRegistry = srv.URL
	skillSearchJSON = false
	defer func() {
		skillSearchRegistry = ""
		skillSearchJSON = false
	}()

	if err := runSkillSearch(skillSearchCmd, []string{"research"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out.String(), "research-and-write") {
		t.Errorf("search missing expected hit:\n%s", out.String())
	}
}

func TestSkillSearch_JSON(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	var out bytes.Buffer
	skillSearchCmd.SetOut(&out)
	skillSearchCmd.SetErr(&out)
	skillSearchRegistry = srv.URL
	skillSearchJSON = true
	defer func() {
		skillSearchRegistry = ""
		skillSearchJSON = false
	}()

	if err := runSkillSearch(skillSearchCmd, nil); err != nil {
		t.Fatalf("search --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("search json output not parseable: %v\n%s", err, out.String())
	}
	if got["total"].(float64) != 2 {
		t.Errorf("expected total=2, got %v", got["total"])
	}
}

func TestSkillSearch_NoResults(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	var out bytes.Buffer
	skillSearchCmd.SetOut(&out)
	skillSearchCmd.SetErr(&out)
	skillSearchRegistry = srv.URL
	defer func() { skillSearchRegistry = "" }()

	if err := runSkillSearch(skillSearchCmd, []string{"xyzzy-no-such-thing"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out.String(), "No skills matched") {
		t.Errorf("expected no-match message:\n%s", out.String())
	}
}

func TestSkillRegister_PrintsYAMLSnippet(t *testing.T) {
	var out bytes.Buffer
	skillRegisterCmd.SetOut(&out)
	skillRegisterCmd.SetErr(&out)
	skillRegisterGitURL = "https://github.com/alice/research.git"
	skillRegisterDescription = "Research"
	skillRegisterTags = []string{"research", "writing"}
	skillRegisterGH = false
	defer func() {
		skillRegisterGitURL = ""
		skillRegisterDescription = ""
		skillRegisterTags = nil
	}()

	if err := runSkillRegister(skillRegisterCmd, []string{"alice/research"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !strings.Contains(out.String(), "handle: alice") {
		t.Errorf("snippet missing handle:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "git_url: https://github.com/alice/research.git") {
		t.Errorf("snippet missing git_url:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "tags: [research, writing]") {
		t.Errorf("snippet missing tags:\n%s", out.String())
	}
}

func TestSkillRegister_RequiresGitURL(t *testing.T) {
	var out bytes.Buffer
	skillRegisterCmd.SetOut(&out)
	skillRegisterCmd.SetErr(&out)
	skillRegisterGitURL = ""
	if err := runSkillRegister(skillRegisterCmd, []string{"alice/research"}); err == nil {
		t.Errorf("expected --git-url required error")
	}
}

func TestSkillRate_SubmitsToEndpoint(t *testing.T) {
	hits := 0
	var bodyBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rating" {
			http.NotFound(w, r)
			return
		}
		hits++
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		bodyBytes = body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var out bytes.Buffer
	skillRateCmd.SetOut(&out)
	skillRateCmd.SetErr(&out)
	skillRateRegistry = srv.URL
	skillRateStars = 4
	defer func() {
		skillRateRegistry = ""
		skillRateStars = 0
	}()
	if err := runSkillRate(skillRateCmd, []string{"alice/research"}); err != nil {
		t.Fatalf("rate: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected one POST to /rating, got %d", hits)
	}
	if !strings.Contains(string(bodyBytes), "\"stars\":4") {
		t.Errorf("rating body missing stars:%s", bodyBytes)
	}
}

func TestSkillRate_InvalidStars(t *testing.T) {
	skillRateStars = 6
	defer func() { skillRateStars = 0 }()
	err := runSkillRate(skillRateCmd, []string{"alice/research"})
	if err == nil {
		t.Errorf("--stars=6 should error")
	}
}

func TestSkillRate_TolerantOfBadEndpoint(t *testing.T) {
	var out bytes.Buffer
	skillRateCmd.SetOut(&out)
	skillRateCmd.SetErr(&out)
	skillRateRegistry = "http://127.0.0.1:1"
	skillRateStars = 5
	defer func() {
		skillRateRegistry = ""
		skillRateStars = 0
	}()
	if err := runSkillRate(skillRateCmd, []string{"alice/research"}); err != nil {
		t.Errorf("rate should be tolerant of bad endpoint: %v", err)
	}
	if !strings.Contains(out.String(), "may be offline") {
		t.Errorf("expected offline message:\n%s", out.String())
	}
}

func TestYAMLQuoteIfNeeded(t *testing.T) {
	cases := map[string]string{
		"simple":                 "simple",
		"with spaces and an end": "with spaces and an end",
		"contains: colon":        "'contains: colon'",
		"says 'hi'":              "'says ''hi'''",
		"multi\nline":            "'multi\nline'",
	}
	for in, want := range cases {
		if got := yamlQuoteIfNeeded(in); got != want {
			t.Errorf("yamlQuoteIfNeeded(%q) = %q, want %q", in, got, want)
		}
	}
}
