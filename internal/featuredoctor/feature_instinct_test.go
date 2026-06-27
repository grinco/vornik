package featuredoctor

import (
	"context"
	"testing"
)

type stubConfig struct{ vals map[string]any }

func (s stubConfig) GateValue(k string) (any, bool) { v, ok := s.vals[k]; return v, ok }

type stubModels struct{ reachable bool }

func (s stubModels) Reachable(context.Context, string) bool { return s.reachable }

func TestInstinctPrereq_ModelReachable(t *testing.T) {
	f := instinctFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"instinct.model": "qwen3.6:35b", "instinct.enabled": true}},
		Models: stubModels{reachable: false},
	}
	var modelCheck *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "distill model reachable" {
			modelCheck = &f.Prereqs[i]
		}
	}
	if modelCheck == nil {
		t.Fatal("instinct feature missing 'distill model reachable' prereq")
	}
	res := modelCheck.Check(context.Background(), deps)
	if res.OK {
		t.Fatal("unreachable model must report prereq unmet")
	}
	if res.Fixable {
		t.Fatal("model reachability is operator-fixable=false (can't pull a model for them)")
	}
}

func TestInstinctConsumerPrereq_RequiresEnabled(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Config: stubConfig{vals: map[string]any{"instinct.enabled": false}}}
	var dep *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "instinct.enabled set" {
			dep = &f.Prereqs[i]
		}
	}
	if dep == nil {
		t.Fatal("missing 'instinct.enabled set' prereq")
	}
	res := dep.Check(context.Background(), deps)
	if res.OK || !res.Fixable {
		t.Fatalf("instinct.enabled=false must be unmet+fixable, got %+v", res)
	}
}
