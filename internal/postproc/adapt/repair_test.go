package adapt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestVerifierRepairer_RoundTripIsByteExact(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		damage func(t *testing.T, dir string)
		slices int
	}{
		{"flips at capacity", "single_small", func(t *testing.T, dir string) {
			flipByte(t, filepath.Join(dir, "single.bin"), 10)
			flipByte(t, filepath.Join(dir, "single.bin"), 5000)
		}, 2},
		{"deleted file", "multi_files", func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "exact.bin")))
		}, 4},
		{"truncated file", "limited_volumes", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "l2.bin")
			fi, err := os.Stat(p)
			require.NoError(t, err)
			require.NoError(t, os.Truncate(p, fi.Size()/2))
		}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, m := goldenDir(t, tt.golden)
			tt.damage(t, dir)

			out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
			require.NoError(t, err)
			require.True(t, out.NeedsRepair)
			require.True(t, out.Repairable)
			require.Equal(t, tt.slices, out.DamagedSlices)

			rep, err := Repairer{}.Repair(context.Background(), dir, out)
			require.NoError(t, err)
			assert.Equal(t, tt.slices, rep.RepairedSlices)
			requireManifestFiles(t, dir, m)

			again, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
			require.NoError(t, err)
			assert.True(t, again.Clean)
		})
	}
}

func TestRepairer_InsufficientRecoveryIsAShortfallAndChangesNothing(t *testing.T) {
	dir, _ := goldenDir(t, "single_small")
	for _, off := range []int64{0, 4096, 8192} {
		flipByte(t, filepath.Join(dir, "single.bin"), off)
	}
	before := digests(t, dir)
	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)

	_, err = Repairer{}.Repair(context.Background(), dir, out)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientRecovery)
	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 3, short.Needed)
	assert.Equal(t, 2, short.Available)
	assert.Equal(t, before, digests(t, dir), "a refused repair must not touch anything")
}

func TestRepairer_WithoutOpaqueIsAnError(t *testing.T) {
	for name, opaque := range map[string]any{"nil": nil, "wrong type": "x", "nil payload": (*verified)(nil), "empty payload": &verified{}} {
		t.Run(name, func(t *testing.T) {
			_, err := Repairer{}.Repair(context.Background(), t.TempDir(), core.VerifyOutcome{NeedsRepair: true, Opaque: opaque})
			require.Error(t, err)
			assert.NotErrorIs(t, err, core.ErrRepairFailed)
			assert.NotErrorIs(t, err, core.ErrInsufficientRecovery)
		})
	}
}

func TestRepairer_CancelledContextPassesThroughAndChangesNothing(t *testing.T) {
	dir, _ := goldenDir(t, "single_small")
	flipByte(t, filepath.Join(dir, "single.bin"), 10)
	before := digests(t, dir)
	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Repairer{}.Repair(ctx, dir, out)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, core.ErrRepairFailed)
	assert.Equal(t, before, digests(t, dir))
}

func TestMapRepairError(t *testing.T) {
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
		{"remaining damage", &par2.RemainingDamageError{Result: &par2.Result{DamagedSlices: 1}},
			[]error{core.ErrRepairFailed, par2.ErrRepairFailed}, []error{core.ErrInsufficientRecovery}, nil},
		{"singular", par2.ErrSingularRecovery,
			[]error{core.ErrRepairFailed, par2.ErrSingularRecovery}, []error{core.ErrInsufficientRecovery}, nil},
		{"too large", fmt.Errorf("w: %w", par2.ErrTooLarge),
			[]error{core.ErrRepairFailed, par2.ErrTooLarge}, nil, nil},
		{"canceled", context.Canceled, []error{context.Canceled}, []error{core.ErrRepairFailed}, nil},
		{"deadline wrapped", fmt.Errorf("w: %w", context.DeadlineExceeded),
			[]error{context.DeadlineExceeded}, []error{core.ErrRepairFailed}, nil},
		{"invalid result is internal", par2.ErrInvalidResult,
			[]error{par2.ErrInvalidResult}, []error{core.ErrRepairFailed, core.ErrInsufficientRecovery}, nil},
		{"incomplete set is internal", par2.ErrIncompleteSet,
			[]error{par2.ErrIncompleteSet}, []error{core.ErrRepairFailed}, nil},
		{"other", boom, []error{boom}, []error{core.ErrRepairFailed, core.ErrInsufficientRecovery}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapRepairError(context.Background(), tt.in)
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
