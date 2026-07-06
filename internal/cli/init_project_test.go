package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseParamFlags_RepeatedKeyAccumulates(t *testing.T) {
	got, err := parseParamFlags([]string{"feeds=a", "feeds=b", "topic=x"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got["feeds"], "repeated keys must accumulate in order")
	assert.Equal(t, []string{"x"}, got["topic"], "singleton keys must be wrapped in a slice")
}

func TestParseParamFlags_TrimsValues(t *testing.T) {
	got, err := parseParamFlags([]string{"feeds=  https://a.example  ", "feeds=\thttps://b.example\r\n"})
	require.NoError(t, err)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, got["feeds"])
}
