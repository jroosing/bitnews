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

func TestCategoryDirOverrideMustBeAbsolute(t *testing.T) {
	c := core.Category{ID: core.NewID(), Name: "tv", DirOverride: "media/tv"}
	if err := c.Validate(); err == nil {
		t.Fatal("relative DirOverride accepted")
	}
	c.DirOverride = "/data/usenet/complete/series"
	if err := c.Validate(); err != nil {
		t.Fatalf("absolute DirOverride: %v", err)
	}
}
