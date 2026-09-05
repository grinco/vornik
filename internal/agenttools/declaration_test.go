package agenttools

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// These tests are the invariants that used to be prose in three files and a
// lint comparing hand-mirrored copies. They run over the declaration itself.

func TestTools_UniqueAndGrammatical(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Tools {
		if seen[tool.Name] {
			t.Errorf("%s declared twice", tool.Name)
		}
		seen[tool.Name] = true
		if !BuiltinName.MatchString(tool.Name) {
			t.Errorf("%q does not match BuiltinName %s", tool.Name, BuiltinName)
		}
	}
	// Declaration order is advertisement order (the golden fixtures pin the
	// model-visible order); the Never tool sits last so the offered list reads
	// as the model sees it.
	if last := Tools[len(Tools)-1]; last.Advertise != AdvertiseNever {
		t.Errorf("last declared tool %s is offered; Never tools go last", last.Name)
	}
}

func TestTools_ExemptReasonIffExempt(t *testing.T) {
	for _, tool := range Tools {
		exempt := tool.Gate == GateExemptByDesign
		if exempt && tool.ExemptReason == "" {
			t.Errorf("%s is exempt with no recorded reason — an exemption is a security decision recorded as data", tool.Name)
		}
		if !exempt && tool.ExemptReason != "" {
			t.Errorf("%s carries an exempt reason but is allowlist-gated", tool.Name)
		}
	}
	for _, p := range UngatedPrefixes {
		if p.Reason == "" {
			t.Errorf("prefix %q exempted with no reason", p.Prefix)
		}
	}
}

// The baseline is read-only by construction (operator ruling 2026-08-14). A
// tool that writes, executes, or leaves the deployment must never be granted
// everywhere without the argument being made again — this is where it is
// re-read.
func TestTools_NothingThatActsIsAlwaysGranted(t *testing.T) {
	for _, tool := range Tools {
		if tool.Acts && tool.AlwaysGranted {
			t.Errorf("%s acts and is always granted", tool.Name)
		}
	}
	// And the ruling's two are still there.
	want := []string{"memory_search", "skill_fetch"}
	if got := AlwaysGranted(); !reflect.DeepEqual(got, want) {
		t.Errorf("AlwaysGranted() = %v, want %v", got, want)
	}
}

// Every declared tool has exactly one schema and every schema names a declared
// tool. The schema's function.name is the tool name, so a copy-pasted file
// cannot carry another tool's definition under a new filename.
func TestTools_SchemasMatchDeclaration(t *testing.T) {
	files, err := schemaFiles()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if names := Names(); !reflect.DeepEqual(files, names) {
		t.Errorf("schema files %v\n!= declared names %v", files, names)
	}
	for _, tool := range Tools {
		var def struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(tool.Schema, &def); err != nil {
			t.Errorf("%s: schema does not parse: %v", tool.Name, err)
			continue
		}
		if def.Type != "function" || def.Function.Name != tool.Name {
			t.Errorf("%s: schema is type=%q name=%q", tool.Name, def.Type, def.Function.Name)
		}
		if def.Function.Description == "" || len(def.Function.Parameters) == 0 {
			t.Errorf("%s: schema lacks a description or parameters", tool.Name)
		}
	}
}

func TestAdvertiseTokens_ClosedAndDistinct(t *testing.T) {
	toks := AdvertiseTokens()
	seen := map[string]bool{}
	for _, tok := range toks {
		if tok == "" || seen[tok] {
			t.Errorf("advertise token %q empty or duplicated", tok)
		}
		seen[tok] = true
	}
	if Advertise(len(toks)).Token() != "" {
		t.Error("an out-of-range Advertise must have no token")
	}
	for _, tool := range Tools {
		if tool.Advertise.Token() == "" {
			t.Errorf("%s has an Advertise with no token", tool.Name)
		}
	}
}

// Offerable is the container's BUILTIN_TOOL_NAMES_JSON. The one declared tool
// it excludes is the one another path appends by name.
func TestOfferable_ExcludesOnlyNever(t *testing.T) {
	off := map[string]bool{}
	for _, n := range Offerable() {
		off[n] = true
	}
	for _, tool := range Tools {
		if off[tool.Name] == (tool.Advertise == AdvertiseNever) {
			t.Errorf("%s: offerable=%v advertise=%s", tool.Name, off[tool.Name], tool.Advertise.Token())
		}
	}
	if !off["tool_result_read"] || off["tool_search"] {
		t.Errorf("Offerable() = %v", Offerable())
	}
}

// The grammar is the providers' function-name grammar, so its complement is
// exactly what can never have been advertised. Both sides pinned, per class.
func TestIsWellFormedName(t *testing.T) {
	for _, ok := range []string{
		"file_read", "file_writepath", "write_file", "glob",
		"mcp__google-workspace__drive_search", "mcp__atlassian__searchJiraIssuesUsingJql",
		"mcp__some-future-vendor__doThing2", "A", "a-b_C9",
		string(make([]byte, 0)) + "x" + repeat("y", 127), // 128 chars
	} {
		if !IsWellFormedName(ok) {
			t.Errorf("IsWellFormedName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"", "file_write</think>allowed_paths", "file_read(path=\"x\")", "ls -la project/</arg_value>",
		"mcp__scraper__web_fetch:fetch", "mcp__some/path/tool", "file.read", "read file",
		"file\tread", "file\nread", "fil€_read", "x=y", "a?b", "a@b", "a+b",
		"x" + repeat("y", 128), // 129 chars
	} {
		if IsWellFormedName(bad) {
			t.Errorf("IsWellFormedName(%q) = true, want false", bad)
		}
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestHelperNames_SortedAndDeclared — the helper set is a derived view of the
// Runtime field, sorted, never nil (it is emitted as a JSON array), and every
// name in it is a declared tool whose dispatch the helper owns.
func TestHelperNames_SortedAndDeclared(t *testing.T) {
	names := HelperNames()
	if names == nil {
		t.Fatal("HelperNames must never be nil — it becomes a JSON array")
	}
	for i, n := range names {
		if i > 0 && names[i-1] >= n {
			t.Errorf("HelperNames not sorted at %d: %v", i, names)
		}
		if !RunsInHelper(n) || !IsBuiltin(n) {
			t.Errorf("%q is in HelperNames but is not a declared RuntimeHelper tool", n)
		}
	}
	for _, tool := range Tools {
		if tool.Runtime == RuntimeShell && RunsInHelper(tool.Name) {
			t.Errorf("%q is RuntimeShell yet RunsInHelper", tool.Name)
		}
	}
}
