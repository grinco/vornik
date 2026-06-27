package mcp

import (
	"strings"
	"testing"
)

func TestReadSSEJSONRPCResponse_PreservesErrorResponse(t *testing.T) {
	// A JSON-RPC error reply with the matching id must be returned (not
	// filtered) so the caller can surface resp.Error — Task 2 relies on this.
	stream := `data: {"jsonrpc":"2.0","id":5,"error":{"code":-32600,"message":"Invalid Request"}}` + "\n\n"
	resp, err := readSSEJSONRPCResponse(strings.NewReader(stream), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error == nil || resp.Error.Message != "Invalid Request" {
		t.Errorf("error response not preserved: %+v", resp)
	}
}

func TestReadSSEJSONRPCResponse_HandlesCRLF(t *testing.T) {
	// HTTP/1.1 servers may use \r\n; bufio.ScanLines strips the trailing \r.
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\r\n\r\n"
	resp, err := readSSEJSONRPCResponse(strings.NewReader(stream), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != 2 {
		t.Errorf("got id=%d, want 2", resp.ID)
	}
}

func TestReadSSEJSONRPCResponse_SkipsNotificationsReturnsMatch(t *testing.T) {
	stream := "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n\n" +
		"event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n\n"
	resp, err := readSSEJSONRPCResponse(strings.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != 7 || string(resp.Result) != `{"ok":true}` {
		t.Errorf("got id=%d result=%s, want id=7 result={\"ok\":true}", resp.ID, resp.Result)
	}
}

func TestReadSSEJSONRPCResponse_NoMatchIsError(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	if _, err := readSSEJSONRPCResponse(strings.NewReader(stream), 99); err == nil {
		t.Error("expected error when no event matches the wanted id, got nil")
	}
}

func TestReadSSEJSONRPCResponse_MultiLineDataAndEOFFlush(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\n" + "data: \"id\":3,\"result\":{\"v\":1}}"
	resp, err := readSSEJSONRPCResponse(strings.NewReader(stream), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != 3 {
		t.Errorf("got id=%d, want 3", resp.ID)
	}
}
