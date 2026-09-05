package agent_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"vornik.io/vornik/internal/llmreplay"
)

// TestEntrypointReplay runs images/vornik-agent/entrypoint.sh against a
// replay server fed from a recorded step (llm-exchange record/replay design
// §5.4): the model is replayed, the tools execute against the case's
// workspace, and result.json must equal what the first replay of the
// recording produced, modulo the wall clock.
//
// Each case is a directory under fixtures/replay/<name>/ holding
// recording.jsonl (the `vornikctl execution exchanges --export` file),
// task.json (the executor's input, copied out of the step's temp root while
// the container ran), a workspace/ tree, expected_result.json, a README
// naming the capture's task id, and — when the role saw MCP tools —
// mcp_catalog.json, the `mcp-bridge discover` output captured from the
// running container. A hand-authored recording would prove the replayer,
// not the loop, so the fixture must come from a real run.
//
// The harness reproduces the container's process environment, not only its
// filesystem (the 2026-09-05 WORKSPACE incident is why that sentence is
// here): the daemon/memory URLs point at a stub that answers 404, because
// the entrypoint advertises some tools only when those URLs are set; the
// task/project/execution ids come from task.json; the model name is the
// recording's, because the request bytes — and result.json's byte-derived
// usage fields — depend on it; and a stub mcp-bridge serves the captured
// catalog for `discover` and refuses everything else, so a replay whose model
// calls an MCP tool diverges loudly.
//
// Set VORNIK_REPLAY_RECORD=1 to write a missing expected_result.json from the
// replay's output; the run then fails on purpose so a recording pass is never
// mistaken for a green one.
func TestEntrypointReplay(t *testing.T) {
	// An ambient registry would replay a different schema environment. The
	// harness must explicitly choose the case snapshot or current registry.
	t.Setenv("VORNIK_TOOL_REGISTRY", filepath.Join(t.TempDir(), "must-not-be-used.sh"))
	_, thisFile, _, _ := runtime.Caller(0)
	here := filepath.Dir(thisFile)
	cases, _ := filepath.Glob(filepath.Join(here, "fixtures", "replay", "*", "recording.jsonl"))
	if len(cases) == 0 {
		t.Skip("no recording captured yet — see 2026-09-04-llm-exchange-record-replay-design.md §5.4")
	}
	helperDir := buildHelper(t)
	entrypoint := filepath.Join(here, "..", "..", "images", "vornik-agent", "entrypoint.sh")
	for _, recPath := range cases {
		caseDir := filepath.Dir(recPath)
		t.Run(filepath.Base(caseDir), func(t *testing.T) {
			f, err := os.Open(recPath)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := llmreplay.Load(f)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			replay := llmreplay.NewServer(rec)
			srv := httptest.NewServer(replay)
			defer srv.Close()
			// The daemon and memory APIs: present (so the entrypoint advertises
			// what it advertised in the container) and empty (404 to everything).
			apiStub := httptest.NewServer(http.NotFoundHandler())
			defer apiStub.Close()

			tmp := t.TempDir()
			ws := filepath.Join(tmp, "ws")
			if out, err := exec.Command("cp", "-a", filepath.Join(caseDir, "workspace"), ws).CombinedOutput(); err != nil {
				t.Fatalf("copy workspace: %v\n%s", err, out)
			}
			if err := os.MkdirAll(filepath.Join(tmp, "out"), 0o755); err != nil {
				t.Fatal(err)
			}
			binDir := filepath.Join(tmp, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if catalog := filepath.Join(caseDir, "mcp_catalog.json"); fileExists(catalog) {
				writeMCPBridgeStub(t, binDir, catalog)
			}
			ids := taskIdentity(t, filepath.Join(caseDir, "task.json"))
			// The role's environment first (env.json: the container's VORNIK_* /
			// AGENT_* variables minus secrets, URLs and ids — tool budget, token
			// caps, model name, all of which shape the request), then the
			// harness's own values, which win because Go keeps the last
			// duplicate.
			env := append(os.Environ(), fixtureEnv(t, filepath.Join(caseDir, "env.json"))...)
			model := fixtureModel(t, caseDir, recPath)
			cmd := exec.Command("bash", entrypoint)
			env = append(env,
				"WORKSPACE="+ws,
				"INPUT_FILE="+filepath.Join(caseDir, "task.json"),
				"OUTPUT_FILE="+filepath.Join(tmp, "out", "result.json"),
				"VORNIK_LLM_ENDPOINT="+srv.URL,
				"VORNIK_LLM_MODEL="+model,
				"VORNIK_LLM_API_KEY=replay",
				"VORNIK_API_URL="+apiStub.URL,
				"VORNIK_MEM_URL="+apiStub.URL,
				"VORNIK_TASK_ID="+ids.TaskID,
				"VORNIK_PROJECT_ID="+ids.ProjectID,
				"VORNIK_EXECUTION_ID="+ids.Workflow.ExecutionID,
				"VORNIK_HELPER_DIR="+helperDir,
				"VORNIK_TOOL_REGISTRY="+fixtureToolRegistry(caseDir, filepath.Join(filepath.Dir(entrypoint), "tool_registry.generated.sh")),
				"PATH="+binDir+string(os.PathListSeparator)+helperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("entrypoint: %v\n%s", err, out)
			}
			st := replay.Stats()
			if st.Missed != 0 {
				t.Errorf("replay missed %d request(s) — the loop diverged from the recording\n%s", st.Missed, out)
			}
			if st.Served != len(rec.Entries) {
				t.Errorf("served %d of %d recorded exchanges", st.Served, len(rec.Entries))
			}
			got, err := os.ReadFile(filepath.Join(tmp, "out", "result.json"))
			if err != nil {
				t.Fatalf("result.json: %v\n%s", err, out)
			}
			expectedPath := filepath.Join(caseDir, "expected_result.json")
			if !fileExists(expectedPath) && os.Getenv("VORNIK_REPLAY_RECORD") == "1" {
				if err := os.WriteFile(expectedPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("recorded %s from this replay — re-run without VORNIK_REPLAY_RECORD to check it", expectedPath)
			}
			want, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !sameResult(got, want) {
				t.Errorf("result.json differs from the recorded run:\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// sameResult compares two result.json documents with the one field the wall
// clock owns removed: diagnostics.durationSeconds. Every other field is either
// the model's (replayed) or derived from the request bytes, which the recorded
// model name keeps identical.
func sameResult(a, b []byte) bool {
	var va, vb map[string]any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	for _, v := range []map[string]any{va, vb} {
		if d, ok := v["diagnostics"].(map[string]any); ok {
			delete(d, "durationSeconds")
		}
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return bytes.Equal(ca, cb)
}

type taskIDs struct {
	TaskID    string `json:"taskId"`
	ProjectID string `json:"projectId"`
	Workflow  struct {
		ExecutionID string `json:"executionId"`
	} `json:"workflow"`
}

func taskIdentity(t *testing.T, path string) taskIDs {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids taskIDs
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatalf("task.json: %v", err)
	}
	return ids
}

// fixtureEnv reads env.json — an object of environment variables the
// container had — as KEY=VALUE pairs. Absent is fine: a fixture recorded
// with the role's defaults needs none.
func fixtureEnv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("env.json: %v", err)
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// fixtureModel is the model the replay runs under: the container's
// VORNIK_LLM_MODEL from env.json, else the recording's first request's model
// (the export omits it today — the canonical form drops it), else "replay".
// The request bytes carry the name, so it has to be the recorded one for the
// byte-derived usage fields of result.json to match.
func fixtureModel(t *testing.T, caseDir, recPath string) string {
	t.Helper()
	for _, kv := range fixtureEnv(t, filepath.Join(caseDir, "env.json")) {
		if len(kv) > len("VORNIK_LLM_MODEL=") && kv[:len("VORNIK_LLM_MODEL=")] == "VORNIK_LLM_MODEL=" {
			return kv[len("VORNIK_LLM_MODEL="):]
		}
	}
	return recordedModel(t, recPath)
}

// recordedModel is the model name of the recording's first request, when the
// export carries one.
func recordedModel(t *testing.T, recPath string) string {
	t.Helper()
	f, err := os.Open(recPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var line struct {
			Request struct {
				Model string `json:"model"`
			} `json:"request"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("recording line 1: %v", err)
		}
		if line.Request.Model != "" {
			return line.Request.Model
		}
		break
	}
	return "replay"
}

// writeMCPBridgeStub puts an mcp-bridge on PATH that answers `discover` with
// the captured catalog and refuses every other verb, so the tools array the
// entrypoint builds is the one the container built, and an MCP call the
// recording did not make fails instead of pretending.
func writeMCPBridgeStub(t *testing.T, binDir, catalog string) {
	t.Helper()
	script := "#!/bin/sh\ncase \"$1\" in\n  discover) cat '" + catalog + "' ;;\n  *) echo \"ERROR: replay harness: mcp-bridge $1 is not served by the fixture\"; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "mcp-bridge"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// fixtureToolRegistry reproduces the recorded schema environment, when pinned.
// It never rewrites requests or weakens canonical matching; current schemas
// are covered independently by tool_definitions_golden_test.sh (§5.4).
func fixtureToolRegistry(caseDir, current string) string {
	snapshot := filepath.Join(caseDir, "tool_registry.generated.sh")
	if fileExists(snapshot) {
		return snapshot
	}
	return current
}

func TestFixtureToolRegistry(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(t.TempDir(), "current.sh")
	if got := fixtureToolRegistry(dir, current); got != current {
		t.Fatalf("missing snapshot: %s", got)
	}
	snapshot := filepath.Join(dir, "tool_registry.generated.sh")
	if err := os.WriteFile(snapshot, []byte("# recorded schema environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fixtureToolRegistry(dir, current); got != snapshot {
		t.Fatalf("snapshot ignored: %s", got)
	}
}
