package retention

import (
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestSweeperWarn_LogsTableNameAndError(t *testing.T) {
	var buf strings.Builder
	logger := zerolog.New(&buf)
	s := &Sweeper{logger: logger}
	s.warn("tasks", errors.New("boom"))
	out := buf.String()
	if !strings.Contains(out, "tasks") {
		t.Errorf("log missing table name: %s", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("log missing error message: %s", out)
	}
	if !strings.Contains(out, "retention sweep failed on table") {
		t.Errorf("log missing message string: %s", out)
	}
}

func TestSweeperWarn_NilReceiverIsSafe(t *testing.T) {
	var s *Sweeper
	// must not panic on nil receiver
	s.warn("any", errors.New("x"))
}
