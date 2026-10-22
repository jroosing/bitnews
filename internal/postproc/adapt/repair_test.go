package adapt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestVerifierRepairer_RoundTripIsByteExact(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, s *testSet)
		slices int
	}{
		{"flips", func(t *testing.T, s *testSet) {
			s.flipByte(t, "alpha.bin", 10)
			s.flipByte(t, "alpha.bin", 5000)
		}, 2},
		{"deleted file", func(t *testing.T, s *testSet) {
			require.NoError(t, os.Remove(s.path("beta.dat")))
		}, 3},
		{"truncated file", func(t *testing.T, s *testSet) {
			require.NoError(t, os.Truncate(s.path("alpha.bin"), 3500))
		}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := makeSet(t, 6, twoFiles)
			tt.damage(t, s)

			out, err := verifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
			require.NoError(t, err)
			require.True(t, out.NeedsRepair)
			require.True(t, out.Repairable)
			require.Equal(t, tt.slices, out.DamagedSlices)

			rep, err := repairer(t).Repair(context.Background(), s.dir, out)
			require.NoError(t, err)
			assert.Equal(t, tt.slices, rep.RepairedSlices)
			s.requireOriginals(t)

			again, err := verifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
			require.NoError(t, err)
			assert.True(t, again.Clean)
			ents, err := os.ReadDir(s.dir)
			require.NoError(t, err)
			assert.Len(t, ents, 2+3, "no name.1 backups left next to the two files and three .par2 files")
		})
	}
}

func TestRepairer_InsufficientRecoveryIsAShortfallAndChangesNothing(t *testing.T) {
	s := makeSet(t, 2, map[string]int{"single.bin": 3000})
	for _, off := range []int{0, 1000, 2000} {
		s.flipByte(t, "single.bin", off)
	}
	before := digests(t, s.dir)
	out, err := verifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
	require.NoError(t, err)

	_, err = repairer(t).Repair(context.Background(), s.dir, out)

	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientRecovery)
	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 3, short.Needed)
	assert.Equal(t, 2, short.Available)
	assert.Equal(t, before, digests(t, s.dir), "a refused repair must not touch anything")
}

func TestRepairer_DoesNotNeedTheOpaquePayload(t *testing.T) {
	s := makeSet(t, 2, map[string]int{"single.bin": 3000})
	s.flipByte(t, "single.bin", 10)

	_, err := repairer(t).Repair(context.Background(), s.dir, core.VerifyOutcome{NeedsRepair: true})

	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestRepairer_CancelledContextPassesThroughAndChangesNothing(t *testing.T) {
	s := makeSet(t, 2, map[string]int{"single.bin": 3000})
	s.flipByte(t, "single.bin", 10)
	before := digests(t, s.dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repairer(t).Repair(ctx, s.dir, core.VerifyOutcome{})

	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, core.ErrRepairFailed)
	assert.Equal(t, before, digests(t, s.dir))
}

func TestRepairer_UnsafeNameIsAPathViolation(t *testing.T) {
	s := makeSet(t, 2, map[string]int{"single.bin": 3000})
	patchName(t, s.path("set.par2"), "single.bin", "../escaped")

	_, err := repairer(t).Repair(context.Background(), s.dir, core.VerifyOutcome{})

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
		{"insufficient", &par2.InsufficientError{Needed: 7, Available: 3},
			[]error{core.ErrInsufficientRecovery}, []error{core.ErrRepairFailed}, &core.RecoveryShortfall{Needed: 7, Available: 3}},
		{"wrapped insufficient", fmt.Errorf("x: %w", &par2.InsufficientError{Needed: 2, Available: 1}),
			[]error{core.ErrInsufficientRecovery}, nil, &core.RecoveryShortfall{Needed: 2, Available: 1}},
		{"repair failed", fmt.Errorf("%w: exit 5", par2.ErrRepairFailed),
			[]error{core.ErrRepairFailed, par2.ErrRepairFailed}, []error{core.ErrInsufficientRecovery}, nil},
		{"unsafe set", fmt.Errorf("%w: name", par2.ErrUnsafeSet),
			[]error{core.ErrPathViolation, par2.ErrUnsafeSet}, []error{core.ErrRepairFailed}, nil},
		{"canceled", context.Canceled, []error{context.Canceled}, []error{core.ErrRepairFailed}, nil},
		{"deadline wrapped", fmt.Errorf("w: %w", context.DeadlineExceeded),
			[]error{context.DeadlineExceeded}, []error{core.ErrRepairFailed}, nil},
		{"no set is internal", par2.ErrNoSet,
			[]error{par2.ErrNoSet}, []error{core.ErrRepairFailed, core.ErrInsufficientRecovery}, nil},
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
