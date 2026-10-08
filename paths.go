package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// runtimeDir returns a private (0700) per-user directory holding the socket,
// the daemon lock and the daemon log.
func runtimeDir() (string, error) {
	dir := os.Getenv("ONCE_RUNTIME_DIR")
	if dir == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, fmt.Sprintf("once-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is not owned by the current user", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func socketPath(dir string) string { return filepath.Join(dir, "once.sock") }
