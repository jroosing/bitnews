package coretest_test

import (
	"bytes"
	"crypto/sha1"
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
)

func TestBuildTorrentHashesTheInfoDictionary(t *testing.T) {
	data, h := coretest.BuildTorrent("n", []coretest.TorrentEntry{{Path: []string{"a"}, Size: 20000}, {Path: []string{"d", "b"}, Size: 1}},
		coretest.WithPrivate(), coretest.WithAnnounce("http://t/a", "http://u/b"))

	_, rest, _ := bytes.Cut(data, []byte("4:info"))
	info := rest[:len(rest)-1] // the closing "e" of the outer dictionary
	assert.Equal(t, sha1.Sum(info), [20]byte(h))
	assert.Contains(t, string(data), "7:privatei1e")
	assert.Contains(t, string(data), "13:announce-listll10:http://t/a10:http://u/bee")
}
