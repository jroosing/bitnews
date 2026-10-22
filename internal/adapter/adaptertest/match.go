package adaptertest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Compare checks got (a decoded JSON value) against want (a decoded JSON
// value that may contain matchers, see the package comment) and returns one
// readable line per difference: the JSON path, what was wanted and what was
// there. It returns nil when they match.
func Compare(want, got any) []string {
	var diffs []string
	compare("$", want, got, &diffs)
	return diffs
}

func compare(path string, want, got any, diffs *[]string) {
	add := func(format string, args ...any) {
		*diffs = append(*diffs, path+": "+fmt.Sprintf(format, args...))
	}
	switch w := want.(type) {
	case string:
		compareString(path, w, got, diffs)
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			add("want an object, got %s", show(got))
			return
		}
		exact := false
		if v, has := w["$exact"]; has {
			exact, _ = v.(bool)
		}
		for _, k := range sortedKeys(w) {
			if k == "$exact" {
				continue
			}
			gv, present := g[k]
			if s, isStr := w[k].(string); isStr && s == "$absent" {
				if present {
					*diffs = append(*diffs, fmt.Sprintf("%s.%s: want it absent, got %s", path, k, show(gv)))
				}
				continue
			}
			if !present {
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: missing (want %s)", path, k, show(w[k])))
				continue
			}
			compare(path+"."+k, w[k], gv, diffs)
		}
		if exact {
			for _, k := range sortedKeys(g) {
				if _, has := w[k]; !has {
					*diffs = append(*diffs, fmt.Sprintf("%s.%s: unexpected key, got %s", path, k, show(g[k])))
				}
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			add("want an array, got %s", show(got))
			return
		}
		if len(w) != len(g) {
			add("want %d array elements, got %d", len(w), len(g))
			return
		}
		for i := range w {
			compare(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], diffs)
		}
	default: // number, bool, null
		if !jsonEqual(want, got) {
			add("want %s, got %s", show(want), show(got))
		}
	}
}

func compareString(path, want string, got any, diffs *[]string) {
	add := func(format string, args ...any) {
		*diffs = append(*diffs, path+": "+fmt.Sprintf(format, args...))
	}
	switch {
	case want == "$any":
	case strings.HasPrefix(want, "$type:"):
		if t := typeOf(got); t != strings.TrimPrefix(want, "$type:") {
			add("want type %s, got %s (%s)", strings.TrimPrefix(want, "$type:"), t, show(got))
		}
	case strings.HasPrefix(want, "$re:"):
		s, ok := got.(string)
		if !ok {
			add("want a string matching /%s/, got %s", strings.TrimPrefix(want, "$re:"), show(got))
			return
		}
		re, err := regexp.Compile(strings.TrimPrefix(want, "$re:"))
		if err != nil {
			add("bad matcher %q: %v", want, err)
		} else if !re.MatchString(s) {
			add("want a string matching /%s/, got %s", re, show(got))
		}
	default:
		if strings.HasPrefix(want, "$$") {
			want = want[1:]
		}
		if s, ok := got.(string); !ok || s != want {
			add("want %s, got %s", show(want), show(got))
		}
	}
}

func typeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

// show renders a value for a diff line: JSON, shortened.
func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// lookup follows a path like "$.nzo_ids[0]" or "$.queue.slots[1].nzo_id" in
// a decoded JSON value and returns what it finds as a string.
func lookup(v any, path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "$")
	if !ok {
		return "", fmt.Errorf("path %q does not start with $", path)
	}
	for rest != "" {
		switch rest[0] {
		case '.':
			end := strings.IndexAny(rest[1:], ".[")
			key := rest[1:]
			if end >= 0 {
				key = rest[1 : 1+end]
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return "", fmt.Errorf("path %q: %q is not applied to an object", path, key)
			}
			if v, ok = obj[key]; !ok {
				return "", fmt.Errorf("path %q: no key %q", path, key)
			}
			rest = rest[1+len(key):]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return "", fmt.Errorf("path %q: unclosed [", path)
			}
			n, err := strconv.Atoi(rest[1:end])
			arr, isArr := v.([]any)
			if err != nil || !isArr || n < 0 || n >= len(arr) {
				return "", fmt.Errorf("path %q: no element %s", path, rest[:end+1])
			}
			v = arr[n]
			rest = rest[end+1:]
		default:
			return "", fmt.Errorf("path %q: unexpected %q", path, rest)
		}
	}
	switch s := v.(type) {
	case string:
		return s, nil
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(s), nil
	}
	return "", fmt.Errorf("path %q: value %s is not a string, number or bool", path, show(v))
}
