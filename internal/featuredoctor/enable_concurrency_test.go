package featuredoctor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serializingWriter detects whether two ApplyEnable transactions ran
// concurrently by tracking how many goroutines are inside the
// backup→write→validate critical section at once. Its own state is
// mutex/atomic-protected so the TEST itself is race-free regardless of
// whether the production applyMu serializes the callers.
type serializingWriter struct {
	mu       sync.Mutex
	content  []byte
	inFlight int32
	overlap  int32 // set to 1 if >1 goroutine is ever in-flight together
}

func (w *serializingWriter) enter() {
	if atomic.AddInt32(&w.inFlight, 1) > 1 {
		atomic.StoreInt32(&w.overlap, 1)
	}
}
func (w *serializingWriter) leave() { atomic.AddInt32(&w.inFlight, -1) }

func (w *serializingWriter) Read() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.content))
	copy(out, w.content)
	return out, nil
}

func (w *serializingWriter) Write(data []byte) error {
	w.enter()
	// Hold the section open long enough that an unsynchronised second
	// caller would overlap deterministically.
	time.Sleep(10 * time.Millisecond)
	w.mu.Lock()
	w.content = append([]byte(nil), data...)
	w.mu.Unlock()
	w.leave()
	return nil
}

func (w *serializingWriter) Backup() (string, error) { return "bak", nil }
func (w *serializingWriter) Restore(string) error    { return nil }
func (w *serializingWriter) Validate() error         { return nil }

// TestApplyEnable_SerializesConcurrentApplies is the regression for the
// backup/restore race: two simultaneous enables must not interleave their
// backup→write windows (one's rollback could revert the other's committed
// change). With applyMu the critical sections never overlap and both gate
// changes land; without it they overlap and a write is lost.
func TestApplyEnable_SerializesConcurrentApplies(t *testing.T) {
	writer := &serializingWriter{content: []byte("instinct:\n  enabled: false\n")}

	mkPlan := func(key string) *EnablePlan {
		return &EnablePlan{
			Changes: []GateChange{{Key: key, From: false, To: true}},
			Apply:   ReloadHot,
		}
	}
	feat := func(key string) Feature {
		return Feature{
			ID:     "f-" + key,
			Apply:  ReloadHot,
			Gates:  []Gate{{Key: key, EnableTo: true}},
			Verify: func(_ context.Context, _ Deps) PrereqResult { return PrereqResult{OK: true} },
		}
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"instinct.a": false, "instinct.b": false}}}
	reloader := &fakeReloader{}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, key := range []string{"instinct.a", "instinct.b"} {
		key := key
		go func() {
			defer wg.Done()
			if _, err := ApplyEnable(context.Background(), feat(key), deps, mkPlan(key), writer, reloader); err != nil {
				t.Errorf("ApplyEnable(%s): %v", key, err)
			}
		}()
	}
	wg.Wait()

	if atomic.LoadInt32(&writer.overlap) == 1 {
		t.Fatal("two ApplyEnable transactions overlapped — backup/write window is not serialized")
	}
	// Both gate changes must survive: the second caller read the first's
	// committed content and added its own key (no lost write).
	final, _ := writer.Read()
	for _, want := range []string{"a: true", "b: true"} {
		if !containsSub(string(final), want) {
			t.Errorf("final config missing %q (a write was lost):\n%s", want, final)
		}
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
