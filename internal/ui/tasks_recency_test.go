package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestParseRecencyWindow(t *testing.T) {
	t.Run("empty means no window", func(t *testing.T) {
		got, err := parseRecencyWindow("")
		if err != nil || got != 0 {
			t.Fatalf("got (%v, %v), want (0, nil)", got, err)
		}
	})

	t.Run("allowlisted values resolve", func(t *testing.T) {
		for in, want := range map[string]time.Duration{
			"1h":  time.Hour,
			"24h": 24 * time.Hour,
			"7d":  7 * 24 * time.Hour,
			"30d": 30 * 24 * time.Hour,
		} {
			got, err := parseRecencyWindow(in)
			if err != nil || got != want {
				t.Errorf("%s → (%v, %v), want %v", in, got, err, want)
			}
		}
	})

	// Silently ignoring it would show every task ever and look like the filter
	// had simply matched a lot — which is the exact failure this parameter
	// exists to fix, reintroduced through a typo.
	t.Run("an unknown value is an error, not a silent no-op", func(t *testing.T) {
		if _, err := parseRecencyWindow("48h"); err == nil {
			t.Fatal("unknown window accepted; the page would silently show every task")
		}
	})

	// A free-form duration parser would accept this and turn the filtered view
	// back into the unfiltered one.
	t.Run("a plausible duration outside the allowlist is refused", func(t *testing.T) {
		if _, err := parseRecencyWindow("8760h"); err == nil {
			t.Fatal("a year-long window was accepted")
		}
	})

	t.Run("the error names what is allowed", func(t *testing.T) {
		_, err := parseRecencyWindow("nonsense")
		if err == nil {
			t.Fatal("no error")
		}
		for _, want := range []string{"1h", "24h", "7d", "30d"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not name %s: %v", want, err)
			}
		}
	})
}

// The window must reach the REPOSITORY, not be applied after paging. Filtering
// a page of 20 in Go would still page over every failure ever recorded and show
// whichever handful happened to be recent — the bug with extra steps.
func TestTasks_UpdatedWithinReachesTheQuery(t *testing.T) {
	var seen persistence.TaskFilter
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			seen = f
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))

	req := httptest.NewRequest(http.MethodGet, "/ui/tasks?status=FAILED&updated_within=24h", nil)
	rec := httptest.NewRecorder()
	srv.Tasks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen.UpdatedSince == nil {
		t.Fatal("updated_within did not reach the repository filter")
	}
	if d := time.Since(*seen.UpdatedSince); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("UpdatedSince is %v ago, want ~24h", d)
	}
}

func TestTasks_NoWindowByDefault(t *testing.T) {
	var seen persistence.TaskFilter
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			seen = f
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))

	req := httptest.NewRequest(http.MethodGet, "/ui/tasks?status=FAILED", nil)
	srv.Tasks(httptest.NewRecorder(), req)

	if seen.UpdatedSince != nil {
		t.Error("an unwindowed request grew a window; every existing link would change meaning")
	}
}

// A typo'd window must not silently widen the view back to everything.
func TestTasks_RejectsAnUnknownWindow(t *testing.T) {
	srv := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithOnboardingDetector(alreadyOnboardedDetector()),
	)
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks?status=FAILED&updated_within=48h", nil)
	rec := httptest.NewRecorder()

	srv.Tasks(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an unrecognised window must not fall back to no filter", rec.Code)
	}
}

// The card COUNTS by failed_at; the list must select by failed_at too. Filtering
// on updated_at instead selects a different set — a lease sweep touching a
// months-old FAILED row makes it look freshly broken, which is the bug the
// operator hit twice.
func TestTasks_FailedWithinFiltersOnFailureTimeNotRowTouch(t *testing.T) {
	var seen persistence.TaskFilter
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			seen = f
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))

	req := httptest.NewRequest(http.MethodGet, "/ui/tasks?status=FAILED&failed_within=24h", nil)
	rec := httptest.NewRecorder()
	srv.Tasks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen.FailedSince == nil {
		t.Fatal("failed_within did not reach the repository filter")
	}
	if seen.UpdatedSince != nil {
		t.Error("failed_within also constrained updated_at; the two select different sets " +
			"and conflating them reintroduces the bug")
	}
	if d := time.Since(*seen.FailedSince); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("FailedSince is %v ago, want ~24h", d)
	}
}

func TestTasks_RejectsAnUnknownFailedWindow(t *testing.T) {
	srv := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithOnboardingDetector(alreadyOnboardedDetector()),
	)
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks?status=FAILED&failed_within=90d", nil)
	rec := httptest.NewRecorder()

	srv.Tasks(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
