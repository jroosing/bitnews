// Package tool starts the external post-processing programs, 7-Zip and
// par2cmdline-turbo (ADR-0007). It is the only place in bitnews that starts a
// subprocess for them.
//
// Run never uses a shell: the caller passes the program and an argument list.
// The environment is scrubbed (a fixed PATH-free set plus LC_ALL=C and a
// private TMPDIR), output is size-capped, a timeout and the context stop the
// whole process group, and a password can only travel through stdin.
package tool
