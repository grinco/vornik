package fixitdoctor

import "testing"

func TestParseEnvelope_PlainJSON(t *testing.T) {
	env, err := ParseEnvelope(`{"message":"hi","resolved":false,"actions":[{"kind":"retry_task","label":"retry","params":{"task_id":"t1"}}]}`)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.Message != "hi" || env.Resolved {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if len(env.Actions) != 1 || env.Actions[0].Kind != ActionKindRetryTask {
		t.Fatalf("unexpected actions: %+v", env.Actions)
	}
}

func TestParseEnvelope_FencedJSON(t *testing.T) {
	env, err := ParseEnvelope("```json\n{\"message\":\"hi\",\"resolved\":true}\n```")
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if !env.Resolved {
		t.Fatalf("expected resolved=true, got %+v", env)
	}
}

func TestParseEnvelope_ProseWrappedJSON(t *testing.T) {
	env, err := ParseEnvelope(`Sure! Here you go: {"message":"looking into it","resolved":false} hope that helps`)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.Message != "looking into it" {
		t.Fatalf("expected extracted message, got %+v", env)
	}
}

func TestParseEnvelope_PlainProseFallsBackToMessage(t *testing.T) {
	env, err := ParseEnvelope("Just checking in, no JSON here.")
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.Message != "Just checking in, no JSON here." {
		t.Fatalf("expected prose fallback, got %+v", env)
	}
	if env.Resolved {
		t.Fatalf("prose fallback must not resolve")
	}
}

func TestParseEnvelope_Empty(t *testing.T) {
	if _, err := ParseEnvelope("   "); err == nil {
		t.Fatalf("expected error on empty input")
	}
}
