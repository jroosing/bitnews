//go:build unix

package unpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var oneFile = listingFor([3]string{"a.txt", "6", "A -rw-r--r--"})

func TestExitStatusAndMessageAreMappedToTheErrorClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		stderr string
		want   error
	}{
		{"ok with a warning text is still ok", 0, "WARNING: x", nil},
		{"wrong password", 2, "ERROR: Wrong password : a.txt", ErrPassword},
		{"encrypted headers", 2, "ERROR: x : Cannot open encrypted archive. Wrong password?", ErrPassword},
		{"data error in encrypted file", 2, "ERROR: Data Error in encrypted file. Wrong password? : a.txt", ErrPassword},
		{"unsupported method", 2, "ERROR: Unsupported Method : a.txt", ErrUnsupported},
		{"data error", 2, "ERROR: Data Error : a.txt", ErrCorrupt},
		{"crc failed", 2, "ERROR: CRC Failed : a.txt", ErrCorrupt},
		{"headers error", 2, "ERRORS:\nHeaders Error", ErrCorrupt},
		{"truncated", 2, "ERRORS:\nUnexpected end of archive", ErrCorrupt},
		{"not an archive", 2, "ERRORS:\nIs not archive", ErrCorrupt},
		{"warning is corrupt", 1, "WARNING: Some files were not extracted", ErrCorrupt},
		{"disk full", 2, "ERROR: No space left on device : a.txt", ErrDiskFull},
		{"disk full, other wording", 2, "ERROR: There is not enough space on the disk.", ErrDiskFull},
		{"out of memory status", 8, "", ErrTooLarge},
		{"out of memory text", 2, "ERROR: Can't allocate required memory!", ErrTooLarge},
		{"fatal without a message", 2, "", ErrCorrupt},
		{"user break without the prompt", 255, "Break signaled", nil},
		{"unknown status", 3, "", ErrCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newJail(t)
			fake := fakeSevenZip(t, oneFile, "echo \""+c.stderr+"\" >&2\nmkdir -p \"$out\" && echo hello > \"$out/a.txt\"\nexit "+strconv.Itoa(c.status))

			_, err := unpackWithFake(t, fake, j)

			switch {
			case c.want == nil && c.status == 0:
				require.NoError(t, err)
			case c.want == nil:
				require.Error(t, err)
				require.False(t, errorsIsAny(err, ErrCorrupt, ErrPassword, ErrUnsupported, ErrLimit), "status 255 is not an archive problem: %v", err)
				j.requireContained(t, false)
			default:
				require.ErrorIs(t, err, c.want)
				j.requireContained(t, false)
			}
		})
	}
}

func TestExitStatusSevenIsABugNotAnArchiveProblem(t *testing.T) {
	j := newJail(t)

	_, err := unpackWithFake(t, fakeSevenZip(t, oneFile, "echo 'Command Line Error' >&2; exit 7"), j)

	require.Error(t, err)
	require.False(t, errorsIsAny(err, ErrCorrupt, ErrPassword, ErrUnsupported, ErrLimit))
}

func TestAFailingListingIsClassifiedToo(t *testing.T) {
	script := "#!/bin/sh\necho 'ERROR: x : Cannot open encrypted archive. Wrong password?' >&2\nexit 2\n"
	p := filepath.Join(t.TempDir(), "fake")
	require.NoError(t, os.WriteFile(p, []byte(script), 0o700))
	j := newJail(t)

	_, err := unpackWithFake(t, p, j)

	require.ErrorIs(t, err, ErrPassword)
	j.requireContained(t, false)
}

func TestTheEncryptedHeaderPromptWithoutAPasswordIsAPasswordError(t *testing.T) {
	script := "#!/bin/sh\nprintf '\\nEnter password:\\n'\necho 'Break signaled'\nexit 255\n"
	p := filepath.Join(t.TempDir(), "fake")
	require.NoError(t, os.WriteFile(p, []byte(script), 0o700))

	_, err := unpackWithFake(t, p, newJail(t))

	require.ErrorIs(t, err, ErrPassword)
}

func TestWhatSevenZipWritesIsCheckedBeforeItIsPublished(t *testing.T) {
	cases := map[string]struct {
		snippet string
		want    error
	}{
		"a symlink to an absolute path":               {`mkdir -p "$out"; echo hello > "$out/a.txt"; ln -s /etc "$out/evil"`, ErrPathViolation},
		"a symlink to the parent":                     {`mkdir -p "$out"; echo hello > "$out/a.txt"; ln -s ../.. "$out/evil"`, ErrPathViolation},
		"a dangling symlink":                          {`mkdir -p "$out"; echo hello > "$out/a.txt"; ln -s nothing "$out/evil"`, ErrPathViolation},
		"a symlinked directory with a file inside it": {`mkdir -p "$out"; echo hello > "$out/a.txt"; ln -s /tmp "$out/evil"`, ErrPathViolation},
		"a hardlink":                                  {`mkdir -p "$out"; echo hello > "$out/a.txt"; ln "$out/a.txt" "$out/b.txt"`, ErrPathViolation},
		"a fifo":                                      {`mkdir -p "$out"; echo hello > "$out/a.txt"; mkfifo "$out/pipe"`, ErrPathViolation},
		"more bytes than declared":                    {`mkdir -p "$out"; echo hello-and-more > "$out/a.txt"`, ErrCorrupt},
		"more files than declared":                    {`mkdir -p "$out"; echo hello > "$out/a.txt"; echo x > "$out/extra"`, ErrCorrupt},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			j := newJail(t)

			_, err := unpackWithFake(t, fakeSevenZip(t, oneFile, c.snippet), j)

			require.ErrorIs(t, err, c.want)
			j.requireContained(t, false)
		})
	}
}

func TestPermissionsOfWhatSevenZipWroteAreNormalized(t *testing.T) {
	listing := listingFor([3]string{"d", "0", "D drwxr-xr-x"}, [3]string{"d/f", "2", "A -rwsr-xr-x"}, [3]string{"g", "2", "A -rwxrwxrwx"})
	snippet := `mkdir -p "$out/d"; echo x > "$out/d/f"; echo y > "$out/g"; chmod 4755 "$out/d/f"; chmod 777 "$out/g"; chmod 500 "$out/d"`
	j := newJail(t)

	res, err := unpackWithFake(t, fakeSevenZip(t, listing, snippet), j)

	require.NoError(t, err)
	require.Equal(t, []string{"d/f", "g"}, res.Files)
	for p, want := range map[string]os.FileMode{"d": 0o755, "d/f": 0o644, "g": 0o644} {
		fi, err := os.Lstat(filepath.Join(j.dst, p))
		require.NoError(t, err)
		require.Equal(t, want, fi.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky), p)
	}
}

func TestNamesThatWereNotSafeAreRenamedAndCollisionsRefused(t *testing.T) {
	listing := listingFor([3]string{"x", "2", "A"}, [3]string{"y", "2", "A"})
	t.Run("renamed", func(t *testing.T) {
		j := newJail(t)

		res, err := unpackWithFake(t, fakeSevenZip(t, listing, `mkdir -p "$out"; echo x > "$out/a:b"; echo y > "$out/con.txt"`), j)

		require.NoError(t, err)
		require.Equal(t, []string{"_con.txt", "a_b"}, res.Files)
	})
	t.Run("collide once renamed", func(t *testing.T) {
		j := newJail(t)

		_, err := unpackWithFake(t, fakeSevenZip(t, listing, `mkdir -p "$out"; echo x > "$out/a:b"; echo y > "$out/a_b"`), j)

		require.ErrorIs(t, err, ErrPathViolation)
		j.requireContained(t, false)
	})
	t.Run("a name with control characters", func(t *testing.T) {
		j := newJail(t)

		res, err := unpackWithFake(t, fakeSevenZip(t, listing, `mkdir -p "$out"; echo x > "$out/$(printf 'ev\033il')"; echo y > "$out/y"`), j)

		require.NoError(t, err)
		require.Equal(t, []string{"evil", "y"}, res.Files)
	})
}

func TestTheArgumentsAreFixedWordsAndAbsolutePaths(t *testing.T) {
	// The fake records its arguments, one per line, to a file.
	record := filepath.Join(t.TempDir(), "args")
	snippet := `printf '%s\n' "$@" > ` + record + `; mkdir -p "$out"; echo hello > "$out/a.txt"`
	j := newJail(t)
	dir := t.TempDir()
	vol := writeFile(t, dir, "-so $(touch pwned).7z", []byte("x"))

	_, err := Unpack(t.Context(), oneSet(vol), j.dst, WithSevenZip(fakeSevenZip(t, oneFile, snippet)), WithPassword("s3cr3t-pw"), WithLogger(nil))

	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(readFile(t, record)), "\n")
	require.Equal(t, "x", args[0])
	require.Contains(t, args, "--", "switches end before the archive name")
	require.Equal(t, vol, args[len(args)-1], "the archive is the last argument, as an absolute path")
	require.Equal(t, filepath.Join(j.base, "jail", "out.tmp"), strings.TrimPrefix(argWith(args, "-o"), "-o"))
	require.NoFileExists(t, filepath.Join(dir, "pwned"))
	for _, a := range args {
		require.NotContains(t, a, "s3cr3t-pw", "the password never goes on the command line")
		require.NotContains(t, a, "-p")
	}
}

func argWith(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return a
		}
	}
	return ""
}

func TestThePasswordTravelsOnStdinOnly(t *testing.T) {
	record := filepath.Join(t.TempDir(), "stdin")
	snippet := `cat > ` + record + `; mkdir -p "$out"; echo hello > "$out/a.txt"`
	j := newJail(t)

	_, err := Unpack(t.Context(), fakeSet(t), j.dst, WithSevenZip(fakeSevenZip(t, oneFile, snippet)), WithPassword("päss word"), WithLogger(nil))

	require.NoError(t, err)
	require.Equal(t, "päss word\n", readFile(t, record))
}

func TestWithoutAPasswordStdinIsEmpty(t *testing.T) {
	record := filepath.Join(t.TempDir(), "stdin")
	snippet := `cat > ` + record + `; mkdir -p "$out"; echo hello > "$out/a.txt"`
	j := newJail(t)

	_, err := unpackWithFake(t, fakeSevenZip(t, oneFile, snippet), j)

	require.NoError(t, err)
	require.Empty(t, readFile(t, record))
}

func TestCancellingKillsSevenZipAndLeavesNothingBehind(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	snippet := `mkdir -p "$out"; echo partial > "$out/a.txt"; echo $$ > ` + pidFile + `; sleep 60`
	j := newJail(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for range 200 { // wait until the fake is running
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()

	_, err := Unpack(ctx, fakeSet(t), j.dst, WithSevenZip(fakeSevenZip(t, oneFile, snippet)), WithLogger(nil))

	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errorsIsAny(err, ErrCorrupt, ErrLimit))
	require.Less(t, time.Since(start), 20*time.Second)
	j.requireContained(t, false)
	pid, perr := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	require.NoError(t, perr)
	require.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }, 5*time.Second, 20*time.Millisecond, "7zz must be dead")
}

func TestACancelledContextNeverStartsSevenZip(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	fake := fakeSevenZip(t, oneFile, ``)
	// Replace the listing with one that leaves a trace.
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+ran+"\nexit 0\n"), 0o700))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := newJail(t)

	_, err := Unpack(ctx, fakeSet(t), j.dst, WithSevenZip(fake), WithLogger(nil))

	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, ran)
	j.requireContained(t, false)
}

func TestAListingWithMoreEntriesThanAllowedIsRefused(t *testing.T) {
	var ents [][3]string
	for i := range 50 {
		ents = append(ents, [3]string{"f" + strconv.Itoa(i), "1", "A"})
	}
	j := newJail(t)

	_, err := unpackWithFake(t, fakeSevenZip(t, listingFor(ents...), ``), j, WithMaxEntries(5))

	require.ErrorIs(t, err, ErrTooManyEntries)
	j.requireContained(t, false)
}

func TestAHugeListingIsCutOffByTheOutputCapAndRefused(t *testing.T) {
	// Far more output than maxEntries entries can explain: the runner stops
	// keeping it and the unpack fails instead of parsing a partial listing.
	var ents [][3]string
	long := strings.Repeat("n", 200)
	for i := range 30000 {
		ents = append(ents, [3]string{strconv.Itoa(i) + long, "1", "A"})
	}
	j := newJail(t)

	_, err := unpackWithFake(t, fakeSevenZip(t, listingFor(ents...), ``), j, WithMaxEntries(5))

	require.ErrorIs(t, err, ErrTooManyEntries)
	j.requireContained(t, false)
}
