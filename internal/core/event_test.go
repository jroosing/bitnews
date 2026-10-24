package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/core"
)

func TestEventsReportTheirKind(t *testing.T) {
	tests := []struct {
		event core.Event
		want  core.EventKind
	}{
		{core.DownloadAdded{}, core.KindDownloadAdded},
		{core.DownloadUpdated{}, core.KindDownloadUpdated},
		{core.DownloadProgress{}, core.KindDownloadProgress},
		{core.Fetched{}, core.KindFetched},
		{core.Ready{}, core.KindReady},
		{core.Failed{}, core.KindFailed},
		{core.Removed{}, core.KindRemoved},
		{core.SettingsChanged{}, core.KindSettingsChanged},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.event.Kind())
	}
}

func TestOnlyDownloadProgressIsAProgressEvent(t *testing.T) {
	assert.True(t, core.DownloadProgress{}.Kind().IsProgress())
	lifecycle := []core.Event{
		core.DownloadAdded{}, core.DownloadUpdated{}, core.Fetched{},
		core.Ready{}, core.Failed{}, core.Removed{}, core.SettingsChanged{},
	}
	for _, ev := range lifecycle {
		assert.False(t, ev.Kind().IsProgress(), ev.Kind())
	}
}
