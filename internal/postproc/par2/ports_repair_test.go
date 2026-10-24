package par2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func TestVerifierRepairer_RoundTripIsByteExact(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, s *portSet)
		slices int
	}{
		{"flips", func(t *testing.T, s *portSet) {
			s.flipByte(t, "alpha.bin", 10)
			s.flipByte(t, "alpha.bin", 5000)
		}, 2},
		{"deleted file", func(t *testing.T, s *portSet) {
			require.NoError(t, os.Remove(s.path("beta.dat")))
		}, 3},
		{"truncated file", func(t *testing.T, s *portSet) {
			require.NoError(t, os.Truncate(s.path("alpha.bin"), 3500))
		}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := makePortSet(t, 6, twoFiles)
			tt.damage(t, s)

			out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
			require.NoError(t, err)
			require.True(t, out.NeedsRepair)
			require.True(t, out.Repairable)
			require.Equal(t, tt.slices, out.DamagedSlices)

			rep, err := portRepairer(t).Repair(context.Background(), s.dir)
			require.NoError(t, err)
			assert.Equal(t, tt.slices, rep.RepairedSlices)
			s.requireOriginals(t)

			again, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
			require.NoError(t, err)
			assert.True(t, again.Clean)
			ents, err := os.ReadDir(s.dir)
			require.NoError(t, err)
			assert.Len(t, ents, 2+3, "no name.1 backups left next to the two files and three .par2 files")
		})
	}
}

func TestRepairer_InsufficientRecoveryIsAShortfallAndChangesNothing(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	for _, off := range []int{0, 1000, 2000} {
		s.flipByte(t, "single.bin", off)
	}
	before := portDigests(t, s.dir)
	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
	require.NoError(t, err)
	require.False(t, out.Repairable)

	_, err = portRepairer(t).Repair(context.Background(), s.dir)

	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientRecovery)
	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 3, short.Needed)
	assert.Equal(t, 2, short.Available)
	assert.Equal(t, before, portDigests(t, s.dir), "a refused repair must not touch anything")
}

func TestRepairer_CancelledContextPassesThroughAndChangesNothing(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	s.flipByte(t, "single.bin", 10)
	before := portDigests(t, s.dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := portRepairer(t).Repair(ctx, s.dir)

	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, core.ErrRepairFailed)
	assert.Equal(t, before, portDigests(t, s.dir))
}

func TestRepairer_UnsafeNameIsAPathViolation(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	portPatchName(t, s.path("set.par2"), "single.bin", "../escaped")

	_, err := portRepairer(t).Repair(context.Background(), s.dir)

	assert.ErrorIs(t, err, core.ErrPathViolation)
}

func TestMapPar2Error(t *testing.T) {
	boom := errors.New("disk on fire")
	tests := []struct {
		name  string
		in    error
		is    []error
		isNot []error
		short *core.RecoveryShortfall
	}{
		{"insufficient", &InsufficientError{Needed: 7, Available: 3},
			[]error{core.ErrInsufficientRecovery}, []error{core.ErrRepairFailed}, &core.RecoveryShortfall{Needed: 7, Available: 3}},
		{"wrapped insufficient", fmt.Errorf("x: %w", &InsufficientError{Needed: 2, Available: 1}),
			[]error{core.ErrInsufficientRecovery}, nil, &core.RecoveryShortfall{Needed: 2, Available: 1}},
		{"repair failed", fmt.Errorf("%w: exit 5", ErrRepairFailed),
			[]error{core.ErrRepairFailed, ErrRepairFailed}, []error{core.ErrInsufficientRecovery}, nil},
		{"unsafe set", fmt.Errorf("%w: name", ErrUnsafeSet),
			[]error{core.ErrPathViolation, ErrUnsafeSet}, []error{core.ErrRepairFailed}, nil},
		{"canceled", context.Canceled, []error{context.Canceled}, []error{core.ErrRepairFailed}, nil},
		{"deadline wrapped", fmt.Errorf("w: %w", context.DeadlineExceeded),
			[]error{context.DeadlineExceeded}, []error{core.ErrRepairFailed}, nil},
		{"no set is internal", ErrNoSet,
			[]error{ErrNoSet}, []error{core.ErrRepairFailed, core.ErrInsufficientRecovery}, nil},
		{"other", boom, []error{boom}, []error{core.ErrRepairFailed, core.ErrInsufficientRecovery}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapPar2Error(context.Background(), "step", tt.in)
			for _, e := range tt.is {
				assert.ErrorIs(t, got, e)
			}
			for _, e := range tt.isNot {
				assert.NotErrorIs(t, got, e)
			}
			if tt.short != nil {
				var s *core.RecoveryShortfall
				require.ErrorAs(t, got, &s)
				assert.Equal(t, *tt.short, *s)
			}
		})
	}
}

func TestPAR2Adapters_AssertPorts(t *testing.T) {
	var (
		_ core.Verifier = Verifier{}
		_ core.Verifier = &Verifier{}
		_ core.Repairer = Repairer{}
		_ core.Repairer = &Repairer{}
	)
}
