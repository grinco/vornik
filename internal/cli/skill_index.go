package cli

// Registry index client. The index is a static JSON document
// served from `skills.vornik.io/index.json` (or a self-hosted
// alternative via VORNIK_SKILL_REGISTRY_URL / --registry flag).
//
// Three jobs:
//
//   - Fetch + decode the index.
//   - Resolve a `<handle>/<skill>[@<version>]` triple to a git URL
//     + revision (the SkillIndexResolver interface that
//     skill_sources.go consumes).
//   - Filter for `skill search` — title / description / tag match.
//
// The index lives at `<registry-base>/index.json`. POST endpoints
// `/install` (telemetry) and `/rating` (operator ratings) share
// the same base; skill_install.go and skill_rate.go construct
// those URLs themselves.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultSkillRegistryURL is the canonical hosted index. The
// `--registry` flag and `VORNIK_SKILL_REGISTRY_URL` env both
// override it; the constant exists so the resolver has a
// sensible default for the common operator who just typed
// `vornikctl skill install vadim/foo`.
const DefaultSkillRegistryURL = "https://skills.vornik.io"

// SkillIndex is the on-the-wire shape of `/index.json`.
type SkillIndex struct {
	Version int              `json:"version" yaml:"version"`
	Skills  []SkillIndexItem `json:"skills" yaml:"skills"`
}

// SkillIndexItem is one row from the curated registry. Fields
// mirror the skills.yaml seed file the static-site generator
// reads.
type SkillIndexItem struct {
	Handle       string   `json:"handle" yaml:"handle"`
	Skill        string   `json:"skill" yaml:"skill"`
	GitURL       string   `json:"git_url" yaml:"git_url"`
	Description  string   `json:"description" yaml:"description"`
	Tags         []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Homepage     string   `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	InstallCount int      `json:"install_count,omitempty" yaml:"install_count,omitempty"`
	RatingAvg    float64  `json:"rating_avg,omitempty" yaml:"rating_avg,omitempty"`
	RatingN      int      `json:"rating_n,omitempty" yaml:"rating_n,omitempty"`
}

// SkillIndexClient fetches the registry index via HTTP. Built
// around an interface (SkillIndexResolver in skill_sources.go)
// so tests can sub in a static fake.
type SkillIndexClient struct {
	baseURL string
	client  *http.Client
}

// NewSkillIndexClient picks the index base URL with the
// standard precedence: explicit baseURL > env override > default.
// Empty baseURL is the documented "use my default" signal.
func NewSkillIndexClient(baseURL string) *SkillIndexClient {
	if baseURL == "" {
		baseURL = os.Getenv("VORNIK_SKILL_REGISTRY_URL")
	}
	if baseURL == "" {
		baseURL = DefaultSkillRegistryURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &SkillIndexClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// BaseURL is the resolved index root (no trailing slash). Used
// when skill_install / skill_rate compose the telemetry endpoint.
func (c *SkillIndexClient) BaseURL() string { return c.baseURL }

// Fetch downloads `<base>/index.json` and returns the parsed
// SkillIndex.
func (c *SkillIndexClient) Fetch() (*SkillIndex, error) {
	url := c.baseURL + "/index.json"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vornikctl-skill/1")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var idx SkillIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parse index json: %w", err)
	}
	return &idx, nil
}

// ResolveHandle implements SkillIndexResolver.
//
// version is optional — when empty the latest matching entry
// wins. Today the index carries one entry per (handle, skill),
// so version selection is just "echo it back as the git ref"
// — the resolver clones at that tag. When the index gains a
// per-version row shape, this method gets a real picker.
func (c *SkillIndexClient) ResolveHandle(handle, skill, version string) (gitURL, revision string, err error) {
	idx, err := c.Fetch()
	if err != nil {
		return "", "", err
	}
	for _, item := range idx.Skills {
		if item.Handle == handle && item.Skill == skill {
			if item.GitURL == "" {
				return "", "", fmt.Errorf("index entry for %s/%s has no git_url", handle, skill)
			}
			return item.GitURL, version, nil
		}
	}
	return "", "", fmt.Errorf("no index entry for %s/%s on %s", handle, skill, c.baseURL)
}

// Search returns index entries whose handle, skill, description,
// or any tag contains the query (case-insensitive). Empty query
// returns every entry — useful as a `--list-registry` smoke.
func (c *SkillIndexClient) Search(query string) ([]SkillIndexItem, error) {
	idx, err := c.Fetch()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var matches []SkillIndexItem
	for _, item := range idx.Skills {
		if q == "" || skillIndexMatches(item, q) {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Handle != matches[j].Handle {
			return matches[i].Handle < matches[j].Handle
		}
		return matches[i].Skill < matches[j].Skill
	})
	return matches, nil
}

func skillIndexMatches(item SkillIndexItem, q string) bool {
	if strings.Contains(strings.ToLower(item.Handle), q) {
		return true
	}
	if strings.Contains(strings.ToLower(item.Skill), q) {
		return true
	}
	if strings.Contains(strings.ToLower(item.Description), q) {
		return true
	}
	for _, tag := range item.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}
