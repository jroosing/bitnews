package web

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/core"
)

func srvs(spec ...string) []core.Server {
	var out []core.Server
	for _, s := range spec {
		name, p, _ := strings.Cut(s, ":")
		pr := 0
		for _, c := range p {
			pr = pr*10 + int(c-'0')
		}
		out = append(out, core.Server{ID: core.ID(name), Name: name, Priority: pr})
	}
	return out
}

// apply returns the list order after applying changes, as the repository
// sorts it: priority, then name.
func apply(servers []core.Server, changes map[core.ID]int) []string {
	next := make([]core.Server, len(servers))
	copy(next, servers)
	for i := range next {
		if p, ok := changes[next[i].ID]; ok {
			next[i].Priority = p
		}
	}
	var out []string
	for _, s := range sortedServers(next) {
		out = append(out, s.Name)
	}
	return out
}

func TestMovePriorities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []core.Server
		id      core.ID
		down    bool
		want    []string // order afterwards; nil means "cannot move"
		changed int
	}{
		{"swap with the neighbour above", srvs("a:0", "b:1", "c:2"), "c", false, []string{"a", "c", "b"}, 2},
		{"swap with the neighbour below", srvs("a:0", "b:1", "c:2"), "a", true, []string{"b", "a", "c"}, 2},
		{"first cannot go up", srvs("a:0", "b:1"), "a", false, nil, 0},
		{"last cannot go down", srvs("a:0", "b:1"), "b", true, nil, 0},
		{"unknown id", srvs("a:0"), "zzz", false, nil, 0},
		{"tie: second goes first", srvs("a:1", "b:1"), "b", false, []string{"b", "a"}, 1},
		{"tie of three: last goes up one", srvs("a:1", "b:1", "c:1", "d:5"), "c", false, []string{"a", "c", "b", "d"}, 1},
		{"tie: first goes down", srvs("a:1", "b:1", "c:1"), "a", true, []string{"b", "a", "c"}, 2},
		{"name order would undo a plain swap", srvs("b:1", "c:1", "a:2"), "a", false, []string{"b", "a", "c"}, 1},
		{"gaps are kept", srvs("a:10", "b:20", "c:30"), "b", false, []string{"b", "a", "c"}, 2},
		{"into a group that shares a number", srvs("a:1", "b:1", "z:2"), "z", false, []string{"a", "z", "b"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := movePriorities(tc.servers, tc.id, tc.down)

			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, apply(tc.servers, got))
			assert.Len(t, got, tc.changed, "only what must change changes")
			for _, p := range got {
				assert.GreaterOrEqual(t, p, 0, "priorities never go negative")
			}
		})
	}
}

func TestEverySettingsSectionHasATemplate(t *testing.T) {
	for _, sec := range settingsSections() {
		assert.NotEmpty(t, sec.Template, sec.Slug)
	}
}
