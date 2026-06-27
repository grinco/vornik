package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// readSSEJSONRPCResponse scans a text/event-stream body and returns the
// JSON-RPC response whose id matches wantID. Server-sent notifications and
// unrelated ids are skipped. Returns an error if the stream ends before the
// matching response arrives. Used only by the streamable-http transport;
// the caller bounds the reader (32 MiB total) before passing it in — note
// the scanner's own 32 MiB cap is per single data line, not per body.
//
// wantID must be >= 1: notifications unmarshal to ID 0, so a wantID of 0
// would collide with them. The caller's reqID counter (atomic.Int64.Add(1))
// guarantees wantID >= 1.
func readSSEJSONRPCResponse(r io.Reader, wantID int64) (*jsonRPCResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	var data strings.Builder
	flush := func() (*jsonRPCResponse, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal([]byte(data.String()), &resp); err != nil {
			return nil, false // non-JSON event (e.g. a server log) — skip
		}
		if resp.ID != wantID {
			return nil, false // notification or unrelated response — skip
		}
		return &resp, true
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // event boundary
			if resp, ok := flush(); ok {
				return resp, nil
			}
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(d)
		default:
			// "event:", "id:", "retry:", or ":comment" — ignored.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read sse stream: %w", err)
	}
	if resp, ok := flush(); ok { // final event with no trailing blank line
		return resp, nil
	}
	return nil, fmt.Errorf("sse stream ended without a response for id %d", wantID)
}
