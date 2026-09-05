package contractreg

import (
	"encoding/json"
	"strings"
	"testing"
)

func view(surface string, defs map[string]string) VocabularyView {
	v := VocabularyView{Surface: surface, Schemas: map[string]json.RawMessage{}}
	for n, d := range defs {
		v.Schemas[n] = json.RawMessage(d)
	}
	return v
}

// The container wraps definitions as {"type":"function","function":{…}};
// the dispatcher's chat.Tool marshals to the same wrapper. Both are accepted,
// and so is a bare function object, because the check is about shape, not
// envelope.
func TestCheckSharedToolSchemas_AgreementIsSilent(t *testing.T) {
	a := view("container", map[string]string{
		"memory_search": `{"type":"function","function":{"name":"memory_search","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}}}`,
		"file_read":     `{"type":"function","function":{"name":"file_read","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}`,
	})
	b := view("dispatcher", map[string]string{
		"memory_search": `{"name":"memory_search","description":"search memory, and see memory_forget","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"},"project_id":{"type":"string"}},"required":["query"]}}`,
		"send_email":    `{"name":"send_email","description":"mail","parameters":{"type":"object","properties":{}}}`,
	})
	if got := CheckSharedToolSchemas(a, b); len(got) != 0 {
		t.Fatalf("differing descriptions and one-sided parameters are allowed; got %+v", got)
	}
	if got := SharedToolNames(a, b); len(got) != 1 || got[0] != "memory_search" {
		t.Fatalf("SharedToolNames = %v", got)
	}
}

func TestCheckSharedToolSchemas_TypeAndRequiredDisagreements(t *testing.T) {
	a := view("container", map[string]string{
		"query_api": `{"name":"query_api","description":"call","parameters":{"type":"object","properties":{"provider":{"type":"string"},"query":{"type":"object"},"limit":{"type":"integer"}},"required":["provider"]}}`,
	})
	b := view("dispatcher", map[string]string{
		"query_api": `{"name":"query_api","description":"call","parameters":{"type":"object","properties":{"provider":{"type":"string"},"query":{"type":"string"},"limit":{"type":"integer"}},"required":["provider","limit"]}}`,
	})
	got := CheckSharedToolSchemas(a, b)
	if len(got) != 2 {
		t.Fatalf("want exactly a type finding on query and a required finding on limit, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, `"limit" is required=false on container and required=true on dispatcher`) {
		t.Errorf("required finding: %s", got[0].Detail)
	}
	if !strings.Contains(got[1].Detail, `"query" is "object" on container and "string" on dispatcher`) {
		t.Errorf("type finding: %s", got[1].Detail)
	}
}

func TestCheckSharedToolSchemas_EmptyDescriptionAndUnparseable(t *testing.T) {
	a := view("container", map[string]string{
		"x": `{"name":"x","description":"","parameters":{"type":"object","properties":{}}}`,
		"y": `not json`,
	})
	b := view("dispatcher", map[string]string{
		"x": `{"name":"x","description":"d","parameters":{"type":"object","properties":{}}}`,
		"y": `{"name":"y","description":"d","parameters":{"type":"object","properties":{}}}`,
	})
	got := CheckSharedToolSchemas(a, b)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Name != "x" || !strings.Contains(got[0].Detail, "description") {
		t.Errorf("x: %+v", got[0])
	}
	if got[1].Name != "y" || !strings.Contains(got[1].Detail, "does not parse") {
		t.Errorf("y: %+v", got[1])
	}
}
