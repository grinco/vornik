package api

import (
	"encoding/json"
	"testing"
)

func TestMergeTopLevelJSON(t *testing.T) {
	base := []byte(`{"taskType":"github-event","context":{"prompt":"body"}}`)
	out, err := mergeTopLevelJSON(base, "forge_job", json.RawMessage(`{"repo":"o/r","number":5}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["taskType"]; !ok {
		t.Error("existing keys must be preserved")
	}
	if _, ok := m["context"]; !ok {
		t.Error("context must be preserved")
	}
	var fj struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(m["forge_job"], &fj); err != nil || fj.Repo != "o/r" || fj.Number != 5 {
		t.Errorf("forge_job not merged: %v %+v", err, fj)
	}
	if _, err := mergeTopLevelJSON([]byte("not json"), "k", json.RawMessage(`1`)); err == nil {
		t.Error("invalid base payload should error")
	}
}
