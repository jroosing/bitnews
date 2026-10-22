// Package adapt connects the ports of the Post-processing orchestrator
// (internal/core) to the real implementations: PAR2 verify and repair
// (internal/postproc/par2) and archive detection and extraction
// (internal/postproc/unpack). It also holds the planning half of deferred
// recovery volumes (M3-07), which is pure, and the DeferredVolumes adapter
// that talks to the fetch side through a small controller interface.
//
// The adapters own the error mapping: they translate the sentinel and typed
// errors of the packages they wrap into the core sentinels, so the
// orchestrator can classify a failure with errors.Is without knowing about
// PAR2 or archive libraries. Context errors always pass through unchanged.
package adapt
