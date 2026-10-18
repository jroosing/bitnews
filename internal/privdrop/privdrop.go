// Package privdrop applies the process-wide startup privileges: the file
// creation mask and, when started as root, the drop to PUID/PGID.
package privdrop

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
)

// Options is what Apply needs from the bootstrap configuration.
type Options struct {
	// RunAs is the identity to drop to when started as root. Nil means no drop.
	RunAs *bootstrap.Identity
	// Umask is the file mode creation mask.
	Umask fs.FileMode
}

// Syscalls is the slice of the operating system that Apply drives. The real
// implementation is OS; tests use a fake.
type Syscalls interface {
	Geteuid() int
	Getegid() int
	Umask(mask int) int
	Setgroups(gids []int) error
	Setgid(gid int) error
	Setuid(uid int) error
}

// Apply sets the umask and then, depending on who the process is:
//
//   - root with RunAs set: drops to RunAs and verifies that root cannot be
//     regained, failing otherwise;
//   - root without RunAs (or RunAs 0:0): logs one warning and stays root;
//   - not root with RunAs set: fails, unless the process already runs as
//     RunAs, in which case there is nothing to do;
//   - not root without RunAs: nothing to do.
//
// An error means startup must be aborted: the process may still hold more
// privileges than configured.
func Apply(opts Options, sys Syscalls, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	sys.Umask(int(opts.Umask))

	euid := sys.Geteuid()
	if opts.RunAs == nil {
		if euid == 0 {
			warnRoot(logger)
		}
		return nil
	}

	id := *opts.RunAs
	if euid != 0 {
		if egid := sys.Getegid(); euid != id.UID || egid != id.GID {
			return fmt.Errorf("PUID/PGID (%d:%d) are set but the process runs as %d:%d and is not root: start as root or unset them",
				id.UID, id.GID, euid, egid)
		}
		return nil
	}

	if id.UID == 0 {
		if id.GID != 0 {
			return fmt.Errorf("PUID is 0 (root) but PGID is %d: set both to 0 or drop to a non-root PUID", id.GID)
		}
		warnRoot(logger)
		return nil
	}
	return drop(id, sys)
}

func warnRoot(logger *slog.Logger) {
	logger.Warn("running as root: a parser bug would run with root privileges")
}

// drop gives up root. The order matters: supplementary groups and the gid
// must change while the process still has the right to, so the uid goes last.
func drop(id bootstrap.Identity, sys Syscalls) error {
	if err := sys.Setgroups([]int{id.GID}); err != nil {
		return fmt.Errorf("setgroups(%d): %w", id.GID, err)
	}
	if err := sys.Setgid(id.GID); err != nil {
		return fmt.Errorf("setgid(%d): %w", id.GID, err)
	}
	if err := sys.Setuid(id.UID); err != nil {
		return fmt.Errorf("setuid(%d): %w", id.UID, err)
	}
	if euid := sys.Geteuid(); euid != id.UID {
		return fmt.Errorf("privilege drop failed: euid is %d, want %d", euid, id.UID)
	}
	if egid := sys.Getegid(); egid != id.GID {
		return fmt.Errorf("privilege drop failed: egid is %d, want %d", egid, id.GID)
	}
	if sys.Setuid(0) == nil {
		return errors.New("privilege drop failed: setuid(0) succeeded after dropping root")
	}
	// With a root group as target, Setgid(0) is allowed and proves nothing.
	if id.GID != 0 && sys.Setgid(0) == nil {
		return errors.New("privilege drop failed: setgid(0) succeeded after dropping root")
	}
	return nil
}
