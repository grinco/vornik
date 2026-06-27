package mocks

import (
	"context"
	"errors"
	"testing"
)

func TestPlaceholder(t *testing.T) {
	// Placeholder test - mocks are tested via the packages that use them
}

// TestMockCountChildrenForParents_DefaultsToNil covers the lazy
// default behavior — callers that don't override the func get a
// (nil, nil) response, the call counter still increments. That
// matches the convention of every other mock method.
func TestMockCountChildrenForParents_DefaultsToNil(t *testing.T) {
	m := &MockTaskRepository{}
	got, err := m.CountChildrenForParents(context.Background(), []string{"a", "b"})
	if err != nil || got != nil {
		t.Errorf("default impl should be (nil, nil), got (%v, %v)", got, err)
	}
	if m.CallCount.CountChildrenForParents != 1 {
		t.Errorf("call counter not incremented: %d", m.CallCount.CountChildrenForParents)
	}
}

// TestMockCountChildrenForParents_FuncOverride confirms the overrideable
// func is invoked when set, and that errors propagate up.
func TestMockCountChildrenForParents_FuncOverride(t *testing.T) {
	want := errors.New("scripted")
	m := &MockTaskRepository{
		CountChildrenForParentsFunc: func(ctx context.Context, ids []string) (map[string]int, error) {
			return map[string]int{"x": 7}, want
		},
	}
	got, err := m.CountChildrenForParents(context.Background(), []string{"x"})
	if !errors.Is(err, want) {
		t.Errorf("expected scripted error, got %v", err)
	}
	if got["x"] != 7 {
		t.Errorf("expected scripted map, got %+v", got)
	}
}
