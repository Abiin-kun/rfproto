package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolveStateDir does not create directories, so read-only commands stay read-only.
// Relative XDG values are invalid; explicit -dir values may be relative.
func resolveStateDir(dir string) (string, error) {
	if dir != "" {
		return filepath.Abs(dir)
	}
	if base := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "rfproto"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve rfproto state directory: %w (set HOME, XDG_STATE_HOME, or -dir)", err)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("HOME must be absolute, or set XDG_STATE_HOME or -dir")
	}
	return filepath.Join(home, ".local", "state", "rfproto"), nil
}
