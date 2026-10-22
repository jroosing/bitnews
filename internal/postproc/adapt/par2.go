package adapt

import (
	"context"
	"errors"
	"fmt"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

// verified is the Opaque payload the Verifier hands, through the
// orchestrator, to the Repairer: the loaded set and the verify result Repair
// needs. It is private so nothing else can depend on its shape.
type verified struct {
	set *par2.Set
	res *par2.Result
}

// Verifier implements core.Verifier on top of par2.LoadDir and par2.Verify.
// The zero value is ready to use.
type Verifier struct {
	// DeferredBlocks returns how many recovery blocks the fetch side can
	// still fetch for a Download (volumes not downloaded yet). They count
	// towards Repairable. Optional: nil means none.
	DeferredBlocks func(id core.ID) int
}

var _ core.Verifier = Verifier{}

// Verify loads the PAR2 set in dir and checks the files against it.
//
// A directory without a usable PAR2 set (no .par2 file, or none with a main
// packet) is not an error: it yields HasPAR2 false. Any other failure of the
// par2 package is returned as it is, which the orchestrator treats as an
// internal error.
//
// Like par2.Verify, it renames obfuscated files to their PAR2 names, which a
// second run finds already done.
func (v Verifier) Verify(ctx context.Context, dir string, opts core.VerifyOptions) (core.VerifyOutcome, error) {
	if err := ctx.Err(); err != nil {
		return core.VerifyOutcome{}, err
	}
	set, err := par2.LoadDir(dir)
	if errors.Is(err, par2.ErrNoMainPacket) {
		return core.VerifyOutcome{}, nil
	}
	if err != nil {
		return core.VerifyOutcome{}, fmt.Errorf("load PAR2 set: %w", err)
	}
	var vopts []par2.Option
	if v.DeferredBlocks != nil {
		vopts = append(vopts, par2.WithDeferredRecoveryBlocks(v.DeferredBlocks(opts.DownloadID)))
	}
	res, err := par2.Verify(ctx, dir, set, vopts...)
	if err != nil {
		return core.VerifyOutcome{}, fmt.Errorf("verify against PAR2 set: %w", err)
	}
	needs := res.NeedsRepair()
	return core.VerifyOutcome{
		HasPAR2:        true,
		Clean:          !needs,
		NeedsRepair:    needs,
		DamagedSlices:  res.RecoverySlicesNeeded(),
		RecoveryOnDisk: res.RecoverySlicesOnDisk,
		Repairable:     res.Repairable(),
		Opaque:         &verified{set: set, res: res},
	}, nil
}

// Repairer implements core.Repairer on top of par2.Repair. The zero value is
// ready to use.
type Repairer struct {
	// Options are passed to par2.Repair, for example a memory limit.
	Options []par2.RepairOption
}

var _ core.Repairer = Repairer{}

// Repair rebuilds the damaged files. outcome must be the VerifyOutcome of a
// Verifier for the same dir; its Opaque payload carries the set and result.
//
// Errors: a *par2.InsufficientError becomes a *core.RecoveryShortfall, which
// wraps core.ErrInsufficientRecovery; a remaining-damage, singular-matrix or
// too-large repair becomes core.ErrRepairFailed (the par2 error is kept in
// the chain for the detail); context errors pass through unchanged; anything
// else is returned as a plain error.
func (r Repairer) Repair(ctx context.Context, dir string, outcome core.VerifyOutcome) (core.RepairOutcome, error) {
	v, ok := outcome.Opaque.(*verified)
	if !ok || v == nil || v.set == nil || v.res == nil {
		return core.RepairOutcome{}, fmt.Errorf("adapt: repair needs the Opaque payload of this package's Verifier, got %T", outcome.Opaque)
	}
	rr, err := par2.Repair(ctx, dir, v.set, v.res, r.Options...)
	if err != nil {
		return core.RepairOutcome{}, mapRepairError(ctx, err)
	}
	return core.RepairOutcome{RepairedSlices: rr.RepairedSlices}, nil
}

func mapRepairError(ctx context.Context, err error) error {
	var ins *par2.InsufficientError
	var rem *par2.RemainingDamageError
	switch {
	case errors.As(err, &ins):
		return &core.RecoveryShortfall{Needed: ins.Needed, Available: ins.Available}
	case isContextErr(ctx, err):
		return err
	case errors.As(err, &rem),
		errors.Is(err, par2.ErrRepairFailed),
		errors.Is(err, par2.ErrSingularRecovery),
		errors.Is(err, par2.ErrTooLarge):
		return fmt.Errorf("%w: %w", core.ErrRepairFailed, err)
	}
	return fmt.Errorf("repair: %w", err)
}

// isContextErr reports whether err is the cancellation or deadline of ctx (or
// wraps either).
func isContextErr(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		(ctx.Err() != nil && errors.Is(err, ctx.Err()))
}
