package vote

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse(t *testing.T) {
	v, ok := Parse("confirm")
	assert.True(t, ok)
	assert.Equal(t, Confirm, v)

	v, ok = Parse("refute")
	assert.True(t, ok)
	assert.Equal(t, Refute, v)

	_, ok = Parse("c")
	assert.False(t, ok)
}
