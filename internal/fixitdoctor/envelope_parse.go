package fixitdoctor

import (
	"encoding/json"
	"errors"
	"strings"
)

// ParseEnvelope unmarshals the LLM's emitted JSON into a FixItEnvelope,
// tolerant of surrounding whitespace, ```json fences, and a
// prose preamble/epilogue — mirrors projectwizard.parseEnvelope
// (wizard.go:587): not every provider honours response_format=
// json_schema, so a chatty model's prose is treated as the message
// rather than hard-failing the turn.
func ParseEnvelope(raw string) (*FixItEnvelope, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("fixitdoctor: empty envelope")
	}
	env, err := unmarshalEnvelope(raw)
	if err != nil {
		if obj, ok := firstJSONObject(raw); ok {
			if env2, err2 := unmarshalEnvelope(obj); err2 == nil {
				env, err = env2, nil
			}
		}
	}
	if err != nil {
		// Plain prose — no JSON envelope and no embedded object. Treat
		// as the assistant's chat message rather than 502ing the turn;
		// mirrors the wizard's identical fallback and rationale.
		return &FixItEnvelope{Message: raw, Resolved: false}, nil
	}
	if strings.TrimSpace(env.Message) == "" {
		return nil, errors.New("fixitdoctor: envelope missing required field: message")
	}
	return env, nil
}

func unmarshalEnvelope(s string) (*FixItEnvelope, error) {
	var env FixItEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// firstJSONObject returns the first balanced {...} object in s,
// respecting string literals and escapes. Returns ("", false) when
// there's no complete object. Mirrors projectwizard's helper of the
// same name (wizard.go:639).
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
