package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"

	"github.com/jroosing/bitnews/internal/core"
)

// BootstrapAdminUsername is the username of the admin created from
// BITNEWS_ADMIN_PASSWORD_FILE.
const BootstrapAdminUsername = "admin"

// MaxAdminPasswordFileBytes caps how much of the password file is read.
const MaxAdminPasswordFileBytes = 4 << 10

// Errors from BootstrapAdminFromFile, besides the password rules.
var (
	ErrAdminPasswordFileTooLarge   = errors.New("auth: admin password file is larger than 4 KiB")
	ErrAdminPasswordFileNotRegular = errors.New("auth: admin password file is not a regular file")
)

// BootstrapAdminFromFile creates the admin User from the password file when
// no User exists, and reports whether it did. It returns (false, nil)
// without touching the file when a User already exists. Call it before
// NewSetup, so a bootstrapped admin closes setup and no setup token is
// generated.
//
// The file is read once, capped at 4 KiB, and one trailing newline is
// trimmed. Its content is never logged or put in an error. A file readable by
// group or others only produces a warning.
func BootstrapAdminFromFile(ctx context.Context, path string, users core.UserRepo, logger *slog.Logger) (bool, error) {
	n, err := users.Count(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: count users: %w", err)
	}
	if n > 0 {
		return false, nil
	}
	pw, err := readPasswordFile(path, logger)
	if err != nil {
		return false, err
	}
	if err := ValidatePassword(pw); err != nil {
		return false, fmt.Errorf("auth: admin password file %s: %w", path, err)
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return false, err
	}
	err = users.Create(ctx, core.User{
		ID:           core.NewID(),
		Username:     BootstrapAdminUsername,
		PasswordHash: hash,
		CreatedAt:    nowUTC(),
	})
	if errors.Is(err, core.ErrConflict) {
		return false, nil // lost a race with another creator; setup is closed either way
	}
	if err != nil {
		return false, fmt.Errorf("auth: create admin from file: %w", err)
	}
	logger.Info("admin account created from the admin password file", "username", BootstrapAdminUsername, "path", path)
	return true, nil
}

func readPasswordFile(path string, logger *slog.Logger) (core.Secret, error) {
	// Stat before open, so a FIFO or device cannot block the read.
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("auth: admin password file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("auth: admin password file %s: %w", path, ErrAdminPasswordFileNotRegular)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("auth: admin password file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		logger.Warn("admin password file is readable by group or others; restrict it to the owner (chmod 600)",
			"path", path, "mode", fmt.Sprintf("%04o", st.Mode().Perm()))
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxAdminPasswordFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("auth: read admin password file: %w", err)
	}
	if len(data) > MaxAdminPasswordFileBytes {
		return "", fmt.Errorf("auth: admin password file %s: %w", path, ErrAdminPasswordFileTooLarge)
	}
	if rest, ok := bytes.CutSuffix(data, []byte("\n")); ok {
		data = bytes.TrimSuffix(rest, []byte("\r"))
	}
	return core.Secret(data), nil
}
