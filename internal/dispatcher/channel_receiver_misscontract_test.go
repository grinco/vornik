package dispatcher

// Regression: 2026-08-19 miss-contract normalisation.
//
// ChatMemoryWriteConfirmationRepository.Get used to answer (nil, nil) for a
// conversation with nothing pending; it now answers persistence.ErrNotFound.
// maybeAcknowledgeMemoryWrite logged any error as "pending lookup failed" —
// and since most conversations have no pending row, that turned the common
// case into a warning on every acknowledgement-shaped message.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
)

func TestMaybeAcknowledgeMemoryWrite_absentPendingRowIsNotLoggedAsAFailure(t *testing.T) {
	var buf bytes.Buffer
	rcv := &ChannelReceiver{
		Channel:                  &stubChannel{name: "slack"},
		Agent:                    &Agent{logger: zerolog.New(&buf)},
		MemoryWriteConfirmations: newFakeConfirmRepo(nil),
	}

	rcv.maybeAcknowledgeMemoryWrite(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess-none", SpeakerID: "UALICE", Text: "share it",
	}, "slack:UALICE")

	if strings.Contains(buf.String(), "pending lookup failed") {
		t.Errorf("a conversation with nothing pending was logged as a lookup failure: %s", buf.String())
	}
}
