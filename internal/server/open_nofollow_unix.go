//go:build unix

package server

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens a file for reading with O_NOFOLLOW so the
// kernel itself refuses to traverse a final-component symlink. This
// closes the TOCTOU window between an Lstat that says "regular file"
// and an os.Open that follows a symlink installed in the
// intervening microseconds — the verification and the open are now
// effectively atomic from our perspective.
//
// Unix-only: syscall.O_NOFOLLOW is not defined on Windows. The
// non-unix build sees a different file that falls back to plain
// os.Open. Windows symlinks require elevated privileges to create
// by default, so the fallback is acceptable for the single-user
// local-first deployment this code targets.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// isSymlinkRejection reports whether an openNoFollow error is the
// kernel saying "I refused because the final component was a
// symlink" (ELOOP on Linux/macOS). Used by the raw-file handler to
// distinguish that 400-shaped failure from a real I/O / permission
// problem, which should surface as 500 — collapsing all errors to
// "not a regular file" would hide production issues behind a
// misleading client error.
func isSymlinkRejection(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
