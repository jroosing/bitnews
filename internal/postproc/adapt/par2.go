package adapt

import (
	"context"
	"errors"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/postproc/par2"
)

// verified is the Opaque payload the Verifier hands, through the
// orchestrator, to the Repairer: what par2 reported. It is private so nothing
// else can depend on its shape. The Repairer does not need it (par2 verifies
// again before it repairs, and the directory may have changed since), so a
// restart in between loses nothing.
type verified struct {
	report *par2.Report
}

// Verifier implements core.Verifier by running par2 (ADR-0007). The zero
// value is ready to use and finds par2 on PATH.
type Verifier struct {
	// DeferredBlocks returns how many recovery blocks the fetch side can
	// still fetch for a Download (volumes not downloaded yet). They count
	// towards Repairable. Optional: nil means none.
	DeferredBlocks func(id core.ID) int
	// Par2 runs the program; the zero value is fine.
	Par2 par2.Tool
}

var _ core.Verifier = Verifier{}

// Verify checks the files in dir against the PAR2 set in it.
//
// A directory without a usable PAR2 set (no .par2 file, or none with a main
// packet) is not an error: it yields HasPAR2 false. A set with an unsafe file
// name is core.ErrPathViolation (par2 is not started). Any other failure is
// returned as it is, which the orchestrator treats as an internal error;
// that includes par2 not being installed (wrapping tool.ErrNotFound).
//
// Like par2.Tool.Verify, it renames obfuscated files to their PAR2 names,
// which a second run finds already done.
func (v Verifier) Verify(ctx context.Context, dir string, opts core.VerifyOptions) (core.VerifyOutcome, error) {
	if err := ctx.Err(); err != nil {
		return core.VerifyOutcome{}, err
	}
	rep, err := v.Par2.Verify(ctx, dir)
	if err != nil {
		return core.VerifyOutcome{}, mapPar2Error(ctx, "verify against PAR2 set", err)
	}
	if !rep.HasSet {
		return core.VerifyOutcome{}, nil
	}
	deferred := 0
	if v.DeferredBlocks != nil {
		deferred = max(v.DeferredBlocks(opts.DownloadID), 0)
	}
	return core.VerifyOutcome{
		HasPAR2:        true,
		Clean:          rep.Clean,
		NeedsRepair:    rep.NeedsRepair,
		DamagedSlices:  rep.DamagedBlocks,
		RecoveryOnDisk: rep.RecoveryBlocks,
		Repairable:     rep.Repairable || rep.DamagedBlocks <= rep.RecoveryBlocks+deferred,
		Opaque:         &verified{report: rep},
	}, nil
}

// Repairer implements core.Repairer by running par2. The zero value is ready
// to use.
type Repairer struct {
	// Par2 runs the program; the zero value is fine.
	Par2 par2.Tool
}

var _ core.Repairer = Repairer{}

// Repair rebuilds the damaged files in dir. The outcome of the Verifier is not
// needed: par2 verifies again as the first step of a repair.
//
// Errors: a *par2.InsufficientError becomes a *core.RecoveryShortfall, which
// wraps core.ErrInsufficientRecovery (par2 changed nothing); a failed repair
// becomes core.ErrRepairFailed (the par2 error is kept in the chain for the
// detail); an unsafe set becomes core.ErrPathViolation; context errors pass
// through unchanged; anything else is returned as a plain error.
func (r Repairer) Repair(ctx context.Context, dir string, _ core.VerifyOutcome) (core.RepairOutcome, error) {
	res, err := r.Par2.Repair(ctx, dir)
	if err != nil {
		return core.RepairOutcome{}, mapPar2Error(ctx, "repair", err)
	}
	return core.RepairOutcome{RepairedSlices: res.RepairedBlocks}, nil
}

// mapPar2Error translates an error of the par2 package into the core
// sentinels. what names the step for the fallback message.
func mapPar2Error(ctx context.Context, what string, err error) error {
	var ins *par2.InsufficientError
	switch {
	case errors.As(err, &ins):
		return &core.RecoveryShortfall{Needed: ins.Needed, Available: ins.Available}
	case isContextErr(ctx, err):
		return err
	case errors.Is(err, par2.ErrUnsafeSet):
		return fmt.Errorf("%w: %w", core.ErrPathViolation, err)
	case errors.Is(err, par2.ErrRepairFailed):
		return fmt.Errorf("%w: %w", core.ErrRepairFailed, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// isContextErr reports whether err is the cancellation or deadline of ctx (or
// wraps either).
func isContextErr(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		(ctx.Err() != nil && errors.Is(err, ctx.Err()))
}
