package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerIsASCIIAndCarriesTheToastAndTheEvents(t *testing.T) {
	rec := httptest.NewRecorder()

	trigger(rec, "danger", "Kan “Ünïcode” 😀 niet", map[string]any{"queue-refresh": true})

	h := rec.Header().Get("HX-Trigger")
	for i := range len(h) {
		require.Less(t, h[i], byte(0x80), "a header is read as Latin-1: non-ASCII must be escaped")
	}
	var got struct {
		Toast        map[string]string `json:"toast"`
		QueueRefresh bool              `json:"queue-refresh"`
	}
	require.NoError(t, json.Unmarshal([]byte(h), &got))
	assert.Equal(t, map[string]string{"level": "danger", "message": "Kan “Ünïcode” 😀 niet"}, got.Toast)
	assert.True(t, got.QueueRefresh)
}

func TestTriggerWithoutAMessageSendsOnlyTheEvents(t *testing.T) {
	rec := httptest.NewRecorder()

	trigger(rec, "", "", map[string]any{"queue-refresh": true})

	assert.JSONEq(t, `{"queue-refresh":true}`, rec.Header().Get("HX-Trigger"))
}
