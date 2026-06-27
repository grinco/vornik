package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHumanizeSize_Bytes(t *testing.T) {
	assert.Equal(t, "0 B", humanizeSize(0))
	assert.Equal(t, "512 B", humanizeSize(512))
	assert.Equal(t, "1023 B", humanizeSize(1023))
}

func TestHumanizeSize_Kilobytes(t *testing.T) {
	assert.Equal(t, "1.0 KB", humanizeSize(1024))
	assert.Equal(t, "1.5 KB", humanizeSize(1536))
}

func TestHumanizeSize_Megabytes(t *testing.T) {
	assert.Equal(t, "1.0 MB", humanizeSize(1<<20))
	assert.Equal(t, "3.5 MB", humanizeSize(int64(3.5*float64(1<<20))))
}

func TestHumanizeSize_Gigabytes(t *testing.T) {
	assert.Equal(t, "1.0 GB", humanizeSize(1<<30))
	assert.Equal(t, "2.0 GB", humanizeSize(2<<30))
}

func TestHumanizeSize_Terabytes(t *testing.T) {
	got := humanizeSize(1 << 40)
	assert.Contains(t, got, "TB")
}
