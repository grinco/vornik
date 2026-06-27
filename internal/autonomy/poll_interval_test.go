package autonomy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/registry"
)

// TestProjectLoop_PollIntervalParseFailureLogsWarning pins the
// regression that prompted this test: an operator's
// `pollInterval: "60"` (no unit suffix) silently fell through
// to the 5-minute default because time.ParseDuration rejected
// the value and the loop ate the error. Live evidence:
// vornik-autocoder.yaml had this exact value for weeks; it
// polled every 5 minutes instead of every hour, and nobody
// noticed because the log was clean. With the fix, the loop
// emits a warn-level log naming the project + the unparseable
// raw value + the fallback so the next operator sees the
// problem within seconds of daemon start.
func TestProjectLoop_PollIntervalParseFailureLogsWarning(t *testing.T) {
	var (
		buf bytes.Buffer
		mu  sync.Mutex
	)
	syncedBuf := &lockedWriter{buf: &buf, mu: &mu}
	logger := zerolog.New(syncedBuf)

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithLogger(logger))
	project := &registry.Project{
		ID: "broken-project",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "test",
			PollInterval:    "60", // BAD: missing unit suffix
			MaxTasksPerHour: 10,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	m.wg.Add(1)
	go m.projectLoop(ctx, project)
	cancel()
	m.wg.Wait()

	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	cases := []string{
		"pollInterval failed to parse",
		"broken-project",
		`"60"`, // raw value echoed
	}
	for _, want := range cases {
		if !strings.Contains(logged, want) {
			t.Errorf("missing expected substring %q in log output:\n%s", want, logged)
		}
	}
}

// TestProjectLoop_PollIntervalValidParses confirms the happy
// path still works — a unit-suffixed value drives the ticker
// without warning.
func TestProjectLoop_PollIntervalValidParses(t *testing.T) {
	var (
		buf bytes.Buffer
		mu  sync.Mutex
	)
	syncedBuf := &lockedWriter{buf: &buf, mu: &mu}
	logger := zerolog.New(syncedBuf)

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithLogger(logger))
	project := &registry.Project{
		ID: "ok-project",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "test",
			PollInterval:    "1h",
			MaxTasksPerHour: 10,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	m.wg.Add(1)
	go m.projectLoop(ctx, project)
	cancel()
	m.wg.Wait()

	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	if strings.Contains(logged, "pollInterval failed to parse") {
		t.Errorf("valid pollInterval should NOT log a parse-failure warning; got:\n%s", logged)
	}
	// The startup log should report the parsed interval.
	if !strings.Contains(logged, "interval") || !strings.Contains(logged, "ok-project") {
		t.Errorf("expected startup log naming project + interval; got:\n%s", logged)
	}
}

// lockedWriter serialises writes to the underlying buffer so
// the loop's logging goroutine doesn't race the test's read.
type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
