// Package par2 checks and repairs a Download with the par2 program
// (par2cmdline-turbo 1.5.0, ADR-0007). It replaces the pure-Go PAR2 parser and
// Reed-Solomon code of ADR-0002: there is no fallback, and a missing par2 is
// an error that names the program.
//
// # What runs
//
// Repairer.Repair (core.Repairer) starts one `par2 r` through
// internal/postproc/tool (the only place that starts processes) in the
// Download's directory. `par2 r` verifies, renames files that match a target
// by content, rebuilds damaged and missing files and checks the result, so
// there is no separate verify run:
//
//		par2 r -B<dir> -t<threads> -m<MB> -- <main.par2> @      (extra files on stdin)
//
//	  - -B<dir> is the absolute Download directory. par2 only reads and writes
//	    below it ("Ignoring out of basepath source file").
//	  - -t is GOMAXPROCS (container aware, at most 16) and -m is 512 MB by
//	    default. par2's own default memory is half of the physical RAM, which a
//	    container limit does not change, so we set it. par2 works with -m1; more
//	    memory only means fewer passes over a big set.
//	  - The main file is the smallest .par2 file of the directory (the index
//	    without recovery blocks). Every other .par2 file and every regular file
//	    in the top level of the directory that is not already a target name goes
//	    to par2 as an "extra file" through stdin (`@`), so a directory with many
//	    files cannot overflow the argument list. par2 only loads sibling volumes
//	    that share the main file's prefix; extras are how it also finds volumes
//	    with other names, and how it finds files whose names were obfuscated: it
//	    scans each extra file and matches it to a target by hash.
//	  - A PAR2 file is known by its content as well as its name: before par2
//	    runs, a top-level file that starts with the "PAR2\0PKT" magic but has
//	    no .par2 name (a fully obfuscated post) is renamed to <name>.par2, so
//	    the set is used and the cleanup stage removes it afterwards.
//	  - Never used: -p (purge deletes the .par2 files, so a crash between the
//	    repair and Ready would leave no set to check against), -N and -S (block
//	    skipping), -q (we read the report).
//
// # Exit codes
//
//	0  nothing to repair, or repair complete       -> HasPAR2, RepairedSlices
//	2  repair required but not possible            -> *core.RecoveryShortfall
//	4  insufficient critical data ("Main packet not found") -> HasPAR2 false
//	5  repair failed                               -> core.ErrRepairFailed
//	3, 6, 7, 8 and a kill                          -> error with the output tail
//
// The English text on stdout (LC_ALL=C is set by the runner) only gives the
// counts: "You have X out of Y data blocks available." (damaged blocks are
// Y-X), "You have Z recovery blocks available." (absent when there are none;
// then the "Loaded N new packets including M recovery blocks" lines are
// summed), "You need N more recovery blocks to be able to repair.", "N
// recovery blocks will be used to repair." and the partial copies of
// obfuscated targets (below). Every pattern is a whole line starting with a
// fixed word, never with a file name.
//
// # Backups
//
// When par2 replaces a damaged target it renames the old file to name.1
// (name.2 if that exists). Left there they would be moved into the library,
// because the cleanup stage only deletes .par2 and .nzb files and archives. So
// after every successful run Repair deletes the regular files called
// <target>.<digits> next to a target, unless that name is a target itself. A
// repair that is interrupted leaves such a file and no target or a partial
// one: the next Repair hands the .N file to par2 as an extra file, which uses
// it as a data source, so the repair can be run again.
//
// An obfuscated and damaged file is repaired into the target name and the
// damaged copy stays under its odd name. par2 reports it (`File: "x" - found 9
// of 10 data blocks from "a.bin".`), and after a successful Repair that copy
// is deleted, provided its name is exactly one of the extra files we listed.
//
// # Hostile input
//
// The names inside a PAR2 file are chosen by the uploader and par2 writes
// files under them. par2cmdline-turbo 1.5.0 neutralises some of them itself
// (it percent-encodes ".." components and a leading "/", turns "\" into "/",
// stops a name at a NUL and escapes line breaks) but not all (a drive letter
// or a quote passes), and other versions differ. So before par2 starts, ReadNames
// reads the File Description packets of every .par2 file and CheckNames
// refuses the whole set with ErrUnsafeSet (a core.ErrPathViolation) when any name is empty, absolute,
// has a drive letter, a backslash, NUL or another control character, a ".."
// "." or empty path component, is too long, names a .par2 file, resolves
// outside the directory through a symlink (fsutil.SafeJoin), or when a target
// already exists as anything but a regular file. Control characters are also
// refused so that no name can fake a line of the report we parse.
//
// ReadNames is a minimal packet reader that is deliberately more permissive
// than par2: it looks for the packet magic at every byte offset, inside
// other packets and recovery data too, and accepts every File Description
// whose MD5 verifies, so it sees every name par2 could ever use. Its memory
// and the work it does on damaged candidates are bounded, and a packet it
// cannot bound makes it refuse the file. It has a fuzz target.
//
// Limits: names are checked as they are written; par2 does not turn
// characters that Windows or SMB shares dislike (":" "?" "*") into "_" the way
// fsutil.SafeName does, so such a name ends up on disk as the uploader wrote it.
package par2
