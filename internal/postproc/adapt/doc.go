// Package adapt connects the ports of the Post-processing orchestrator
// (internal/core) to the real implementations: PAR2 verify and repair, which
// run the par2 program (internal/postproc/par2, ADR-0007; Verifier and
// Repairer find par2 on PATH unless their Par2 field says otherwise), and
// archive detection and extraction, which run the 7zz program
// (internal/postproc/unpack, ADR-0007; Unpacker finds 7zz on PATH unless its
// SevenZip field says otherwise). It also holds the planning half of deferred
// recovery volumes (M3-07), which is pure, and the DeferredVolumes adapter
// that talks to the fetch side through a small controller interface.
//
// The adapters own the error mapping: they translate the sentinel and typed
// errors of the packages they wrap into the core sentinels, so the
// orchestrator can classify a failure with errors.Is without knowing about
// PAR2 or archive tools (for PAR2: no recovery data is
// core.RecoveryShortfall, a failed repair core.ErrRepairFailed and an unsafe
// file name in the set core.ErrPathViolation). Context errors always pass through unchanged.
package adapt
