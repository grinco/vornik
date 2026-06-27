package stepoutcome

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutcomeIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome Outcome
		want    bool
	}{
		{name: "ok is terminal", outcome: OK, want: true},
		{name: "parse error is terminal", outcome: ParseError, want: true},
		{name: "pending validation is not terminal", outcome: PendingValidation, want: false},
		{name: "zero value is not terminal", outcome: Outcome(""), want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.outcome.IsTerminal())
		})
	}
}

func TestOutcomeString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ok", OK.String())
	assert.Equal(t, "", Outcome("").String())
	assert.Equal(t, "custom", Outcome("custom").String())
}
