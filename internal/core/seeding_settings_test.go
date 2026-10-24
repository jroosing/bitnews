package core_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func TestDefaultSeedingHasNoLimitAndPausesWhenOneIsReached(t *testing.T) {
	s := core.DefaultSettings().Seeding

	assert.Equal(t, core.SeedingActionPause, s.Action)
	assert.Equal(t, core.UnlimitedShareLimit(), s.ShareLimit(), "no default limit, so Seeding never stops on its own")
	require.NoError(t, core.DefaultSettings().Validate())
}

func TestSeedingSettingsZeroValueIsTheDefault(t *testing.T) {
	// A PUT of settings from a client that does not know the key decodes to
	// the zero value, which must not stop every torrent at once.
	var s core.SeedingSettings

	assert.Equal(t, core.UnlimitedShareLimit(), s.ShareLimit())
	assert.Equal(t, core.SeedingActionPause, s.EffectiveAction())
}

func TestSeedingShareLimitConvertsMinutesAndTreatsNonPositiveAsUnlimited(t *testing.T) {
	s := core.SeedingSettings{RatioLimit: 2, SeedingTimeMinutes: 90}
	assert.Equal(t, core.ShareLimit{Ratio: 2, SeedingTime: 90 * time.Minute}, s.ShareLimit())

	s = core.SeedingSettings{RatioLimit: -1, SeedingTimeMinutes: -1}
	assert.Equal(t, core.UnlimitedShareLimit(), s.ShareLimit())
}

func TestSeedingSettingsValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		s  core.SeedingSettings
		ok bool
	}{
		"each action":         {core.SeedingSettings{Action: core.SeedingActionRemoveData}, true},
		"empty action":        {core.SeedingSettings{}, true},
		"unknown action":      {core.SeedingSettings{Action: "delete_everything"}, false},
		"NaN ratio":           {core.SeedingSettings{RatioLimit: math.NaN()}, false},
		"infinite ratio":      {core.SeedingSettings{RatioLimit: math.Inf(1)}, false},
		"absurd seeding time": {core.SeedingSettings{SeedingTimeMinutes: math.MaxInt32}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := core.DefaultSettings()
			s.Seeding = tc.s
			err := s.Validate()
			if tc.ok {
				require.NoError(t, err)
			} else {
				assert.True(t, errors.Is(err, core.ErrInvalidSettings), "got %v", err)
			}
		})
	}
}

func TestSeedingSettingsUseTheDocumentedJSONKeys(t *testing.T) {
	b, err := json.Marshal(core.DefaultSettings())
	require.NoError(t, err)
	var got map[string]map[string]any
	require.NoError(t, json.Unmarshal(b, &got))

	for _, key := range []string{"ratio_limit", "seeding_time_minutes", "action"} {
		assert.Contains(t, got["seeding"], key)
	}
	assert.Equal(t, "pause", got["seeding"]["action"])
}
