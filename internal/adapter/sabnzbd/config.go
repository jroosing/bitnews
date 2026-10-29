package sabnzbd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
)

// defaultCategoryName is how SAB calls "no category".
const defaultCategoryName = "*"

// categoryDir is the dir a media manager joins to complete_dir to find a
// Category's output: the Category's name, or the override relative to the
// Usenet complete root when it lies inside it, else the absolute override.
// Only the Usenet override counts: SAB is the Usenet adapter.
func categoryDir(root string, c core.Category) string {
	if c.UsenetDir == "" {
		return c.Name
	}
	if rel, err := filepath.Rel(root, c.UsenetDir); err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return c.UsenetDir
}

// getConfig answers mode=get_config. Sonarr and Radarr read the complete dir
// and the categories to find where output lands, and check that SAB's own
// sorting is off (bitnews never renames or moves output into sorted trees).
func (c *call) getConfig() {
	s := c.h.Settings.Current()
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		c.appFail(err)
		return
	}
	root := s.Paths.Usenet.Complete
	out := []configCategory{{Name: defaultCategoryName, PP: "3", Script: "None", Priority: sabPriorityDefault}}
	for i, cat := range cats {
		out = append(out, configCategory{
			Name: cat.Name, Order: i + 1, PP: "3", Script: "Default",
			Dir: categoryDir(root, cat), Priority: sabPriorityDefault,
		})
	}
	c.json(configBody{Config: config{
		Misc: configMisc{
			CompleteDir: root, DownloadDir: s.Paths.Usenet.Incomplete,
			TVCategories: []string{}, MovieCategories: []string{}, DateCategories: []string{},
			HistoryRetentionOption: "all",
		},
		Categories: out,
		Servers:    []struct{}{},
		Sorters:    []struct{}{},
	}})
}

// getCats answers mode=get_cats: "*" first, then the Category names.
func (c *call) getCats() {
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		c.appFail(err)
		return
	}
	names := []string{defaultCategoryName}
	for _, cat := range cats {
		names = append(names, cat.Name)
	}
	c.json(categoriesBody{Categories: names})
}

// fullStatus answers mode=fullstatus and mode=status with the few fields a
// health or disk check reads.
func (c *call) fullStatus() {
	s := c.h.Settings.Current()
	_, paused := c.h.speed()
	free, _ := c.h.disk(s.Paths.Usenet.Complete)
	c.json(fullStatusBody{Status: fullStatus{
		Version: c.h.Version, CompleteDir: s.Paths.Usenet.Complete, DownloadDir: s.Paths.Usenet.Incomplete,
		Paused: paused, PauseInt: "0", HaveWarnings: "0", Warnings: []string{},
		DiskSpace1: gigabytes(free), DiskSpace2: gigabytes(free),
	}})
}

func (h *handler) speed() (bytesPerSec int64, paused bool) {
	if h.SpeedSource == nil {
		return 0, false
	}
	bps, paused := h.SpeedSource()
	return max(bps, 0), paused
}

func (h *handler) disk(path string) (free, total int64) {
	if h.DiskSpace == nil {
		return 0, 0
	}
	f, t, ok := h.DiskSpace(path)
	if !ok {
		return 0, 0
	}
	return max(f, 0), max(t, 0)
}

// gigabytes formats n bytes as SAB's diskspace fields do: "%.2f" GiB.
func gigabytes(n int64) string { return fmt.Sprintf("%.2f", float64(n)/(1<<30)) }
