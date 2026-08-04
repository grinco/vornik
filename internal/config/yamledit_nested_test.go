package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Nested-mapping support exists so a form can write an MCP `auth:` block (MCP server
// authentication design step 6). Before it, every yamledit writer could emit only scalars and
// string lists, which is why the control-plane MCP tab had to refuse editing a server carrying
// one rather than preserve it.

func TestSetYAMLListItemField_WritesANestedMapping(t *testing.T) {
	const in = `mcp:
  servers:
    - name: n8n
      transport: streamable-http
      url: https://n8n.example.com/mcp/abc
`
	out, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "n8n", "auth", map[string]any{
		"mode":         "static",
		"value_from":   "secret://N8N_TOKEN",
		"value_prefix": "Bearer ",
	})
	if err != nil {
		t.Fatalf("SetYAMLListItemField: %v", err)
	}
	got := string(out)
	for _, want := range []string{"auth:", "mode: static", "value_from: secret://N8N_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The value must survive a round trip through the parser, not merely
	// appear in the text.
	var probe struct {
		MCP struct {
			Servers []struct {
				Name string `yaml:"name"`
				Auth struct {
					Mode        string `yaml:"mode"`
					ValueFrom   string `yaml:"value_from"`
					ValuePrefix string `yaml:"value_prefix"`
				} `yaml:"auth"`
			} `yaml:"servers"`
		} `yaml:"mcp"`
	}
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(probe.MCP.Servers) != 1 {
		t.Fatalf("servers = %d", len(probe.MCP.Servers))
	}
	if probe.MCP.Servers[0].Auth.ValueFrom != "secret://N8N_TOKEN" {
		t.Errorf("auth.value_from = %q", probe.MCP.Servers[0].Auth.ValueFrom)
	}
	if probe.MCP.Servers[0].Auth.ValuePrefix != "Bearer " {
		t.Errorf("auth.value_prefix = %q (the trailing space is load-bearing)", probe.MCP.Servers[0].Auth.ValuePrefix)
	}
}

// TestSetYAMLListItemField_NestedMappingKeysAreSorted — a config diff that reshuffles on every
// save is unreviewable, and the control-plane ledger renders these as diffs.
func TestSetYAMLListItemField_NestedMappingKeysAreSorted(t *testing.T) {
	const in = "mcp:\n  servers:\n    - name: s\n      transport: stdio\n"
	first, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "s", "auth", map[string]any{
		"zeta": "1", "alpha": "2", "mode": "env",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "s", "auth", map[string]any{
		"mode": "env", "alpha": "2", "zeta": "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("map iteration order leaked into the output:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if idx1, idx2 := strings.Index(string(first), "alpha"), strings.Index(string(first), "zeta"); idx1 > idx2 {
		t.Errorf("keys are not sorted:\n%s", first)
	}
}

// TestSetYAMLListItemField_ReplacesAnExistingNestedMapping — an edit must overwrite the block, not
// merge into it: a stale key left behind would be a credential reference nobody intended.
func TestSetYAMLListItemField_ReplacesAnExistingNestedMapping(t *testing.T) {
	const in = `mcp:
  servers:
    - name: n8n
      transport: streamable-http
      auth:
        mode: static
        value_from: secret://OLD_TOKEN
        value_prefix: "Bearer "
`
	out, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "n8n", "auth", map[string]any{
		"mode":       "static",
		"value_from": "secret://NEW_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "OLD_TOKEN") {
		t.Errorf("the previous block survived the replace:\n%s", got)
	}
	if strings.Contains(got, "value_prefix") {
		t.Errorf("a key absent from the new block was merged in from the old one:\n%s", got)
	}
	if !strings.Contains(got, "NEW_TOKEN") {
		t.Errorf("the new value did not land:\n%s", got)
	}
}

func TestSetNodeValue_NestedStringMap(t *testing.T) {
	const in = "mcp:\n  servers:\n    - name: reddit\n      transport: stdio\n"
	out, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "reddit", "env_from",
		map[string]string{"REDDIT_CLIENT_ID": "secret://rid"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "REDDIT_CLIENT_ID: secret://rid") {
		t.Errorf("map[string]string was not written:\n%s", out)
	}
}

// TestSetNodeValue_RejectsAnUnsupportedNestedValue keeps the error loud rather than writing a
// half-formed node.
func TestSetNodeValue_RejectsAnUnsupportedNestedValue(t *testing.T) {
	const in = "mcp:\n  servers:\n    - name: s\n      transport: stdio\n"
	_, err := SetYAMLListItemField([]byte(in), "mcp.servers", "name", "s", "auth", map[string]any{
		"scopes": []int{1, 2, 3},
	})
	if err == nil {
		t.Fatal("expected an unsupported-value error")
	}
	if !strings.Contains(err.Error(), "scopes") {
		t.Errorf("error should name the offending key: %v", err)
	}
}
