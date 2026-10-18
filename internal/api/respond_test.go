package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), into), rec.Body.String())
}

// errorBody is the API error shape.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) (code string) {
	t.Helper()
	var b errorBody
	decode(t, rec, &b)
	require.NotEmpty(t, b.Error.Code, rec.Body.String())
	require.NotEmpty(t, b.Error.Message)
	return b.Error.Code
}
