package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestTaskMatchesPalette(t *testing.T) {
	task := &persistence.Task{
		ID:        "task_20260509101010_DEADBEEF1234",
		ProjectID: "assistant-project",
		Status:    persistence.TaskStatusAwaitingInput,
	}

	cases := []struct {
		name       string
		q          string
		suffixHint string
		want       bool
	}{
		{name: "nil_task", q: "task", want: false},
		{name: "full_id_substring", q: "deadbeef", want: true},
		{name: "project_substring", q: "assistant", want: true},
		{name: "status_substring", q: "awaiting", want: true},
		{name: "short_id_suffix_without_prefix", q: "1234", suffixHint: "1234", want: true},
		{name: "short_id_suffix_with_prefix", q: "t-1234", suffixHint: "1234", want: true},
		{name: "no_match", q: "missing", suffixHint: "missing", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			if tc.name == "nil_task" {
				got = taskMatchesPalette(nil, tc.q, tc.suffixHint)
			} else {
				got = taskMatchesPalette(task, tc.q, tc.suffixHint)
			}
			if got != tc.want {
				t.Fatalf("taskMatchesPalette(q=%q, suffixHint=%q) = %v, want %v", tc.q, tc.suffixHint, got, tc.want)
			}
		})
	}
}

func TestPaletteSearchReturnsFilteredStaticActions(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/ui/palette/search?q=cost", nil)
	rr := httptest.NewRecorder()

	s.PaletteSearch(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"label":"Spend"`) {
		t.Fatalf("filtered palette response missing Spend action: %s", body)
	}
	if strings.Contains(body, `"label":"Memory"`) {
		t.Fatalf("filtered palette response should not include unmatched Memory action: %s", body)
	}
}

func TestSSEBusPublishSubscribeAndUnsubscribe(t *testing.T) {
	bus := NewSSEBus()
	subA, unsubscribeA := bus.Subscribe("task-a")
	subB, unsubscribeB := bus.Subscribe("task-b")

	event := SSEEvent{Kind: "status", Data: "<span>running</span>"}
	bus.Publish("task-a", event)

	select {
	case got := <-subA.Events():
		if got != event {
			t.Fatalf("subscriber got %+v, want %+v", got, event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for task-a event")
	}
	select {
	case got := <-subB.Events():
		t.Fatalf("task-b subscriber unexpectedly received %+v", got)
	default:
	}

	unsubscribeA()
	select {
	case _, ok := <-subA.Events():
		if ok {
			t.Fatal("subscriber channel remained open after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscriber channel close")
	}
	unsubscribeB()
}

func TestSSEBusPublishDropsWhenSubscriberBufferIsFull(t *testing.T) {
	bus := NewSSEBus()
	sub, unsubscribe := bus.Subscribe("task-a")
	defer unsubscribe()

	for i := 0; i < cap(sub.ch)+5; i++ {
		bus.Publish("task-a", SSEEvent{Kind: "message", Data: "x"})
	}

	if got := len(sub.ch); got != cap(sub.ch) {
		t.Fatalf("buffer length = %d, want capped at %d", got, cap(sub.ch))
	}
}

func TestSSEBusPublishWithTimeoutHonorsCanceledContext(t *testing.T) {
	bus := NewSSEBus()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	bus.PublishWithTimeout(ctx, "missing-task", SSEEvent{Kind: "status", Data: "x"}, time.Minute)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("PublishWithTimeout took %s with canceled context", elapsed)
	}
}
