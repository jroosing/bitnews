package daemon_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

func TestE2EBothPostProcessOptionsCleanTheNameAndDotTheFilesOfASABnzbdAdd(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	data := payload(20_000, 9)
	fx := nntptest.BuildFixture(t, map[string][]byte{"my_show_s01e01.mkv": data}, 5_000)
	fx.Install(srv.Server)

	var s core.Settings
	e.call("GET", "/api/v1/settings", nil, http.StatusOK, &s)
	s.PostProcess = core.PostProcessOptions{CleanNames: true, UnderscoresToDots: true}
	e.call("PUT", "/api/v1/settings", s, http.StatusOK, nil)

	nzo := e.sabAddFile(e.apiKey, "My_Show_S01E01-GRP-Obfuscated", fx.NZB)
	id := core.ID(strings.TrimPrefix(nzo, "SABnzbd_nzo_"))
	done := e.waitState(id, core.StateReady)

	assert.Equal(t, "My_Show_S01E01-GRP", done.Name, "Clean.py removed -Obfuscated at add time")
	assert.Equal(t, "My_Show_S01E01-GRP", filepath.Base(done.OutputPath))
	assert.Equal(t, map[string][]byte{"my.show.s01e01.mkv": data}, withoutMarker(readTree(t, done.OutputPath)))
}

func withoutMarker(tree map[string][]byte) map[string][]byte {
	delete(tree, ".bitnews-done")
	return tree
}
