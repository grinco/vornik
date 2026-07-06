package projectwizard

import (
	"encoding/json"
	"testing"
)

func TestParamValue_AcceptsStringOrArray(t *testing.T) {
	var scalar ParamValue
	if err := json.Unmarshal([]byte(`"x"`), &scalar); err != nil || len(scalar) != 1 || scalar[0] != "x" {
		t.Fatalf("scalar: %v %v", scalar, err)
	}
	var arr ParamValue
	if err := json.Unmarshal([]byte(`["a","b"]`), &arr); err != nil || len(arr) != 2 {
		t.Fatalf("array: %v %v", arr, err)
	}
}

func TestComposition_ParamsMulti(t *testing.T) {
	c := &Composition{Params: map[string]ParamValue{
		"projectId": {"pricing-watch"},
		"sources":   {"a", "b"},
	}}
	m := c.ParamsMulti()
	if len(m["projectId"]) != 1 || len(m["sources"]) != 2 {
		t.Fatalf("ParamsMulti = %#v", m)
	}
}

func TestEnvelope_UnmarshalsComposition(t *testing.T) {
	raw := `{"message":"ok","ready_to_commit":false,"composition":{"template":"report-pipeline","params":{"projectId":"p","sources":["x","y"]},"addons":[{"type":"schedule","interval":"168h","goal":"g"}]}}`
	var e Envelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Composition == nil || e.Composition.Template != "report-pipeline" ||
		len(e.Composition.Params["sources"]) != 2 || len(e.Composition.Addons) != 1 ||
		e.Composition.Addons[0].Type != "schedule" {
		t.Fatalf("composition not parsed: %+v", e.Composition)
	}
}
