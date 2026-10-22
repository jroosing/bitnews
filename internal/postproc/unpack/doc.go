// Package unpack extracts RAR, 7z and zip archives by running 7-Zip (7zz)
// with path and size guards.
//
// Detect finds the archive sets in a directory by magic bytes; Unpack extracts
// one set into a staging directory and renames it into place. The extraction
// itself is done by the external program 7zz (ADR-0007), started through
// internal/postproc/tool, which is the only package allowed to import os/exec
// (TestOnlyTheToolRunnerImportsOsExec). There is no pure-Go fallback.
//
// # Hostile archives
//
// Every archive is hostile, and so is the output of 7zz about it. Unpack
// therefore wraps the tool in its own checks:
//
//  1. List. "7zz l -slt -ba" prints one block of fields per entry. The parser
//     (listing.go) is strict about the shape, because names and comments are
//     printed into the output: a repeated field, a stray line or a second
//     "Path" line inside one entry fails with ErrCorrupt.
//  2. Guard. Before a byte is written, every entry name goes through cleanName
//     (fsutil.SafeName per component; no "..", absolute paths, drive letters
//     or NUL bytes; no names that collide once case is ignored), only files
//     and directories are accepted (a symlink, hardlink, device, FIFO or
//     socket fails with ErrPathViolation), and the entry count
//     (WithMaxEntries), the declared total size (WithMaxBytes), the
//     compression ratio and the free disk space (WithFreeSpace, WithMinFree)
//     are checked. An encrypted archive without a password fails with
//     ErrPassword.
//  3. Extract. "7zz x -y -bd -aos -o<staging>" runs with the staging directory
//     as its working directory. Multi-volume sets are given by their first
//     volume only; 7zz finds the others (name.partNN.rar, name.rar with
//     name.r00, name.7z.001).
//  4. Walk. The staging tree is checked again, by what is on disk: a symlink,
//     hardlink or special file, an escaping path, more files or bytes than the
//     listing declared, or colliding names fail the Unpack; names that
//     SafeName would change are renamed in place; files become 0644 and
//     directories 0755.
//  5. Publish. The staging directory is renamed to the destination.
//
// # Passwords and errors
//
// A password is written to the stdin of 7zz, which reads it at its prompt. It
// is never an argument, so it does not show in the process list, and it is not
// part of any error. Exit status and message text are mapped to the package's
// errors by classify (sevenzip.go): ErrPassword, ErrUnsupported, ErrCorrupt,
// ErrDiskFull, ErrTooLarge. A cancelled context kills the process group of
// 7zz and removes the staging directory.
//
// What is not guarded: memory. 7zz allocates what an archive's headers and
// dictionaries ask for. The container's memory limit contains that (ADR-0007).
package unpack
