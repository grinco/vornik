package narrator

import "vornik.io/vornik/internal/secrets"

// SecretScanner is the narrow slice of secrets.Detector the narrator
// needs — Scan only. Mirrors the memory.UsageRecorder /
// memory.PricingTable narrow-interface convention so tests supply a
// fake without pulling in the full secrets package surface.
//
// The narrator's PRIMARY defence against leaking secrets (design
// §6) is structural: it only ever reads event METADATA (tool name,
// step role, step id, durations, outcome class) — it never touches
// ToolCallStartedPayload.InputJSON or ToolCallFinishedPayload.
// OutputJSON, so raw tool payloads never reach the prompt or a
// stored line in the first place. Scanner is a defence-in-depth
// backstop over the final composed text (LLM output or template)
// in case a step/tool NAME itself happens to look secret-shaped.
type SecretScanner interface {
	Scan(text []byte) []secrets.Finding
}

// redactLine scans text and returns it with any findings replaced,
// via secrets.Redact. scanner may be nil (skip — the structural
// no-raw-payload guarantee above is what actually matters); an empty
// findings list returns text unchanged.
func redactLine(scanner SecretScanner, text string) string {
	if scanner == nil || text == "" {
		return text
	}
	findings := scanner.Scan([]byte(text))
	if len(findings) == 0 {
		return text
	}
	return string(secrets.Redact([]byte(text), findings))
}
