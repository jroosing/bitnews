package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroosing/bitnews/internal/core"
)

func TestCategoryWithValidNameIsValid(t *testing.T) {
	for _, name := range []string{"tv", "movies-4k", "tv.anime", "a_b", "0", strings.Repeat("x", 64)} {
		c := core.Category{ID: core.NewID(), Name: name}
		if err := c.Validate(); err != nil {
			t.Errorf("Category name %q: %v", name, err)
		}
	}
}

func TestCategoryWithInvalidNameIsRejected(t *testing.T) {
	for _, name := range []string{"", "TV", "tv shows", "tv/anime", "../tv", "série", strings.Repeat("x", 65)} {
		c := core.Category{ID: core.NewID(), Name: name}
		if err := c.Validate(); !errors.Is(err, core.ErrInvalidCategoryName) {
			t.Errorf("Category name %q: got %v, want ErrInvalidCategoryName", name, err)
		}
	}
}

func TestCategoryNamedLikeADirectoryReferenceIsRejected(t *testing.T) {
	for _, name := range []string{".", ".."} {
		c := core.Category{ID: core.NewID(), Name: name}
		if err := c.Validate(); !errors.Is(err, core.ErrInvalidCategoryName) {
			t.Errorf("Category name %q: got %v, want ErrInvalidCategoryName", name, err)
		}
	}
}

func TestCategoryDirOverridesMustBeAbsolute(t *testing.T) {
	for _, c := range []core.Category{
		{ID: core.NewID(), Name: "tv", UsenetDir: "media/tv"},
		{ID: core.NewID(), Name: "tv", TorrentDir: "media/tv"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("relative override accepted: %+v", c)
		}
	}
	c := core.Category{ID: core.NewID(), Name: "tv", UsenetDir: "/data/usenet/complete/series", TorrentDir: "/data/torrents/series"}
	if err := c.Validate(); err != nil {
		t.Fatalf("absolute overrides: %v", err)
	}
}

func TestCategoryDirOverrideIsPerSourceKind(t *testing.T) {
	c := core.Category{Name: "movies", UsenetDir: "/data/usenet/movies", TorrentDir: "/data/torrents/movies"}

	if got := c.DirOverride(core.SourceNZB); got != "/data/usenet/movies" {
		t.Errorf("usenet: %q", got)
	}
	if got := c.DirOverride(core.SourceTorrent); got != "/data/torrents/movies" {
		t.Errorf("torrent: %q", got)
	}
	if got := (core.Category{Name: "tv", UsenetDir: "/data/tv"}).DirOverride(core.SourceTorrent); got != "" {
		t.Errorf("torrent without its own override: %q, want the default", got)
	}
}
