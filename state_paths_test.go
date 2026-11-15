package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateDirectoryXDGAndExplicitOverride(t *testing.T) {
	home, base := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", base)
	dir, err := resolveStateDir("")
	if err != nil || dir != filepath.Join(base, "rfproto") {
		t.Fatalf("XDG state: %q, %v", dir, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative-state")
	dir, err = resolveStateDir("")
	if err != nil || dir != filepath.Join(home, ".local", "state", "rfproto") {
		t.Fatalf("relative XDG fallback: %q, %v", dir, err)
	}
	t.Setenv("HOME", "")
	if _, err := resolveStateDir(""); err == nil {
		t.Fatal("missing home silently selected the working directory")
	}
	explicit := t.TempDir()
	dir, err = resolveStateDir(explicit)
	if err != nil || dir != explicit {
		t.Fatalf("explicit override with no HOME: %q, %v", dir, err)
	}
}

func TestPeerCommandReadsXDGStateWithoutCreatingAnotherStore(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	dir := filepath.Join(base, "rfproto")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peers.json"), []byte(`[{"realDeviceId":"xdg-phone","deviceName":"XDG phone","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}]`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := peersCommand([]string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"realDeviceId":"xdg-phone"`) || strings.Contains(out.String(), `"key"`) {
		t.Fatalf("XDG peer not listed safely: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "peers.lock")); !os.IsNotExist(err) {
		t.Fatalf("read-only list opened writer lock: %v", err)
	}
}
