package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
)

// trigger sets the HX-Trigger header: a toast (level success, info, warn or
// danger; none without a message) plus any other client events. It is the
// only writer of the header. Everything is written as ASCII, with non-ASCII
// characters as JSON \u escapes, because browsers read header values as
// Latin-1 and would garble them.
func trigger(w http.ResponseWriter, level, message string, events map[string]any) {
	m := map[string]any{}
	maps.Copy(m, events)
	if message != "" {
		m["toast"] = map[string]string{"level": level, "message": message}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x7f:
			sb.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	w.Header().Set("HX-Trigger", sb.String())
}
