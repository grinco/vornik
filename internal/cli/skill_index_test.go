package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sampleIndex() SkillIndex {
	return SkillIndex{
		Version: 1,
		Skills: []SkillIndexItem{
			{
				Handle:      "vadim",
				Skill:       "research-and-write",
				GitURL:      "https://github.com/vadim/vornik-skills.git",
				Description: "Two-step researcher → writer pipeline.",
				Tags:        []string{"research", "writing"},
			},
			{
				Handle:      "alice",
				Skill:       "news-feed",
				GitURL:      "https://github.com/alice/news.git",
				Description: "Aggregates morning news feeds.",
				Tags:        []string{"news"},
			},
		},
	}
}

func startFakeIndex(t *testing.T, idx SkillIndex) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(idx)
	})
	return httptest.NewServer(mux)
}

func TestSkillIndexClient_Fetch(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	idx, err := NewSkillIndexClient(srv.URL).Fetch()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if idx.Version != 1 {
		t.Errorf("index version: %d", idx.Version)
	}
	if len(idx.Skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(idx.Skills))
	}
}

func TestSkillIndexClient_FetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "oops", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := NewSkillIndexClient(srv.URL).Fetch()
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("want HTTP 500 error, got %v", err)
	}
}

func TestSkillIndexClient_ResolveHandle(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	c := NewSkillIndexClient(srv.URL)
	url, rev, err := c.ResolveHandle("vadim", "research-and-write", "v1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if url != "https://github.com/vadim/vornik-skills.git" {
		t.Errorf("git url: %q", url)
	}
	if rev != "v1.0.0" {
		t.Errorf("revision: %q", rev)
	}
}

func TestSkillIndexClient_ResolveHandleNotFound(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	_, _, err := NewSkillIndexClient(srv.URL).ResolveHandle("ghost", "missing", "")
	if err == nil || !strings.Contains(err.Error(), "no index entry") {
		t.Errorf("want no-entry error, got %v", err)
	}
}

func TestSkillIndexClient_Search(t *testing.T) {
	srv := startFakeIndex(t, sampleIndex())
	defer srv.Close()

	c := NewSkillIndexClient(srv.URL)

	// keyword match in description
	res, err := c.Search("morning")
	if err != nil || len(res) != 1 || res[0].Skill != "news-feed" {
		t.Errorf("morning search: %v / %#v", err, res)
	}

	// tag match
	res, err = c.Search("research")
	if err != nil || len(res) != 1 || res[0].Skill != "research-and-write" {
		t.Errorf("research search: %v / %#v", err, res)
	}

	// empty query returns everything sorted
	res, err = c.Search("")
	if err != nil || len(res) != 2 {
		t.Fatalf("empty search: %v / %d", err, len(res))
	}
	if res[0].Handle != "alice" {
		t.Errorf("empty search not sorted: %#v", res)
	}
}

func TestSkillIndexClient_DefaultURL(t *testing.T) {
	c := NewSkillIndexClient("")
	if c.BaseURL() != DefaultSkillRegistryURL {
		t.Errorf("default baseURL: %q want %q", c.BaseURL(), DefaultSkillRegistryURL)
	}
}

func TestSkillIndexClient_EnvOverride(t *testing.T) {
	t.Setenv("VORNIK_SKILL_REGISTRY_URL", "https://my.example.com")
	c := NewSkillIndexClient("")
	if c.BaseURL() != "https://my.example.com" {
		t.Errorf("env override ignored: %q", c.BaseURL())
	}
}
