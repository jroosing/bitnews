package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSSEFrameKeepsEveryLineInItsOwnDataField(t *testing.T) {
	got := sseFrame("live", "<div>\r\n  a\n</div>")

	assert.Equal(t, "event: live\ndata: <div>\ndata:   a\ndata: </div>\n\n", got)
}
