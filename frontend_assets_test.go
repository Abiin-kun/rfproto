// frontend_assets_test.go: tests for the read-only wallpaper/status observer.
//
// The observer is a passenger on the phone's authenticated frame stream, so the
// tests feed it hand-built service-25 envelopes and wait for its worker
// goroutine to publish a snapshot. Nothing here talks to a phone, a UI, or the
// network; every write lands in t.TempDir().
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testWallpaperPNG renders a solid, non-uniform PNG of the requested size.
func testWallpaperPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x * 5) % 256), G: uint8((y * 7) % 256), B: 200, A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode %dx%d: %v", width, height, err)
	}
	return buf.Bytes()
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// observerFrame builds the authenticated service-25 envelope the phone pushes,
// with the observer's inner command fields under "data".
func observerFrame(t *testing.T, cmd int, data map[string]any) []byte {
	t.Helper()
	inner := map[string]any{"cmd": cmd}
	for key, value := range data {
		inner[key] = value
	}
	body, err := json.Marshal(map[string]any{"serviceId": 25, "data": inner})
	if err != nil {
		t.Fatalf("marshal service %d cmd %d: %v", 25, cmd, err)
	}
	return body
}

// wallpaperFrame is a command-2 wallpaper push: base64 PNG plus its md5.
func wallpaperFrame(t *testing.T, encoded, advertisedMD5 string) []byte {
	t.Helper()
	return observerFrame(t, 2, map[string]any{"wallpaper": encoded, "md5": advertisedMD5})
}

// statusFrame is a command-1 phone status push.
func statusFrame(t *testing.T, data map[string]any) []byte {
	t.Helper()
	return observerFrame(t, 1, data)
}

// waitForSnapshot polls the observer until cond holds, because a wallpaper push
// is decoded by the observer's worker goroutine.
func waitForSnapshot(t *testing.T, obs *frontendObserver, timeout time.Duration, cond func(frontendSnapshot) bool) frontendSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snap := obs.Snapshot()
		if cond(snap) {
			return snap
		}
		if !time.Now().Before(deadline) {
			return snap
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNewObserverAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	obs := newFrontendObserver(dir)
	if obs == nil {
		t.Fatal("newFrontendObserver returned nil")
	}
	snap := obs.Snapshot()
	if snap.Version != 0 || snap.WallpaperVersion != 0 || snap.WallpaperBytes != 0 ||
		snap.WallpaperWidth != 0 || snap.WallpaperHeight != 0 {
		t.Fatalf("a fresh observer must start empty: %+v", snap)
	}
	if want := filepath.Join(dir, "wallpaper.png"); snap.WallpaperPath != want {
		t.Errorf("wallpaperPath = %q, want %q", snap.WallpaperPath, want)
	}
	if snap.Phone != (phoneProfile{}) {
		t.Errorf("phone = %+v, want the zero profile", snap.Phone)
	}

	// A wallpaper left by an earlier run is adopted as version 1.
	cached := testWallpaperPNG(t, 72, 48)
	if err := os.WriteFile(snap.WallpaperPath, cached, 0o600); err != nil {
		t.Fatalf("seed cached wallpaper: %v", err)
	}
	reloaded := newFrontendObserver(dir).Snapshot()
	if reloaded.WallpaperVersion != 1 || reloaded.Version != 1 {
		t.Errorf("cached wallpaper version = %d/%d, want 1/1", reloaded.WallpaperVersion, reloaded.Version)
	}
	if reloaded.WallpaperWidth != 72 || reloaded.WallpaperHeight != 48 || reloaded.WallpaperBytes != len(cached) {
		t.Errorf("cached wallpaper geometry = %dx%d (%d bytes), want 72x48 (%d bytes)",
			reloaded.WallpaperWidth, reloaded.WallpaperHeight, reloaded.WallpaperBytes, len(cached))
	}
}

func TestObserverIconPathStaysInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	obs := newFrontendObserver(dir)
	icons := filepath.Join(dir, "icons")

	if got, want := obs.IconPath("com.example.app"), filepath.Join(icons, "com.example.app.png"); got != want {
		t.Errorf("IconPath = %q, want %q", got, want)
	}
	for _, escape := range []string{"com.example/../../evil", "..", "/etc/passwd", ".", ""} {
		got := obs.IconPath(escape)
		if got == "" {
			if escape == "" || escape == "." {
				continue
			}
			t.Errorf("IconPath(%q) is empty", escape)
			continue
		}
		if !strings.HasPrefix(got, icons) || strings.Contains(got, "..") {
			t.Errorf("IconPath(%q) = %q escapes the assets directory", escape, got)
		}
	}
}

func TestObserverWallpaperCmd2(t *testing.T) {
	dir := t.TempDir()
	obs := newFrontendObserver(dir)

	raw := testWallpaperPNG(t, 64, 64)
	obs.Observe(25, wallpaperFrame(t, base64.StdEncoding.EncodeToString(raw), md5Hex(raw)))

	snap := waitForSnapshot(t, obs, 2*time.Second, func(s frontendSnapshot) bool { return s.WallpaperBytes > 0 })
	want := filepath.Join(dir, "wallpaper.png")
	if snap.WallpaperPath != want {
		t.Errorf("wallpaperPath = %q, want %q", snap.WallpaperPath, want)
	}
	if snap.WallpaperBytes != len(raw) || snap.WallpaperWidth != 64 || snap.WallpaperHeight != 64 {
		t.Errorf("wallpaper = %d bytes %dx%d, want %d bytes 64x64",
			snap.WallpaperBytes, snap.WallpaperWidth, snap.WallpaperHeight, len(raw))
	}
	if snap.WallpaperVersion != 1 || snap.Version != 1 {
		t.Errorf("version = wallpaper %d / snapshot %d, want 1/1", snap.WallpaperVersion, snap.Version)
	}

	info, err := os.Stat(snap.WallpaperPath)
	if err != nil {
		t.Fatalf("wallpaper file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wallpaper perms = %v, want 0600", perm)
	}
	onDisk, err := os.ReadFile(snap.WallpaperPath)
	if err != nil {
		t.Fatalf("read wallpaper: %v", err)
	}
	if !bytes.Equal(onDisk, raw) {
		t.Error("the cached wallpaper is not byte-identical to the pushed PNG")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(onDisk))
	if err != nil {
		t.Fatalf("cached file is not a PNG: %v", err)
	}
	if cfg.Width != 64 || cfg.Height != 64 {
		t.Errorf("cached PNG is %dx%d, want 64x64", cfg.Width, cfg.Height)
	}

	// The atomic write must not leave its temporary file behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "wallpaper.png" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("directory contents = %v, want [wallpaper.png]", names)
	}
}

func TestObserverStatusCmd1(t *testing.T) {
	dir := t.TempDir()
	obs := newFrontendObserver(dir)

	obs.Observe(25, statusFrame(t, map[string]any{
		"model": "TestPhone", "deviceName": "ignored when model is set",
		"battery": 55, "wifi": true, "wifiSignal": 3, "screen": true,
		"bluetooth": true, "data": true, "isCharging": true, "wallpaperState": 2,
		"ver": "15", "usbReady": false,
	}))

	snap := waitForSnapshot(t, obs, 2*time.Second, func(s frontendSnapshot) bool { return s.Phone.Model != "" })
	phone := snap.Phone
	if phone.Model != "TestPhone" {
		t.Errorf("model = %q, want %q", phone.Model, "TestPhone")
	}
	if phone.Battery != 55 || !phone.HasBattery {
		t.Errorf("battery = %d (has=%t), want 55 (has=true)", phone.Battery, phone.HasBattery)
	}
	if !phone.Wifi || phone.WifiSignal != 3 {
		t.Errorf("wifi = %t signal %d, want true 3", phone.Wifi, phone.WifiSignal)
	}
	if !phone.Screen || !phone.Bluetooth || !phone.MobileData || !phone.Charging {
		t.Errorf("flags = screen:%t bluetooth:%t data:%t charging:%t", phone.Screen, phone.Bluetooth, phone.MobileData, phone.Charging)
	}
	if phone.WallpaperState != 2 || phone.Version != "15" || phone.USB {
		t.Errorf("wallpaperState = %d version = %q usb = %t", phone.WallpaperState, phone.Version, phone.USB)
	}
	if snap.Version != 1 {
		t.Errorf("snapshot version = %d, want 1", snap.Version)
	}

	// A repeated, identical status must not churn the version.
	obs.Observe(25, statusFrame(t, map[string]any{
		"model": "TestPhone", "deviceName": "ignored when model is set",
		"battery": 55, "wifi": true, "wifiSignal": 3, "screen": true,
		"bluetooth": true, "data": true, "isCharging": true, "wallpaperState": 2,
		"ver": "15", "usbReady": false,
	}))
	if again := obs.Snapshot(); again.Version != snap.Version {
		t.Errorf("unchanged status bumped the version to %d", again.Version)
	}

	// The device name covers a missing model.
	obs.Observe(25, statusFrame(t, map[string]any{"deviceName": "Fallback phone", "battery": 90}))
	snap = waitForSnapshot(t, obs, 2*time.Second, func(s frontendSnapshot) bool { return s.Phone.Model == "Fallback phone" })
	if snap.Phone.Battery != 90 || !snap.Phone.HasBattery {
		t.Errorf("fallback profile = %+v", snap.Phone)
	}
	if snap.Version != 2 {
		t.Errorf("snapshot version = %d, want 2 after one change", snap.Version)
	}

	// Only service 25 is observed; other services are ignored entirely.
	before := obs.Snapshot()
	obs.Observe(1, statusFrame(t, map[string]any{"model": "Other service"}))
	obs.Observe(0, []byte(`{"data":{"cmd":1,"model":"Other service"}}`))
	if after := obs.Snapshot(); after != before {
		t.Errorf("a foreign service changed the snapshot: %+v", after)
	}

	// A frame without a command, or with no data at all, is dropped quietly.
	obs.Observe(25, []byte(`{"data":{"model":"no command"}}`))
	obs.Observe(25, []byte(`not json`))
	if after := obs.Snapshot(); after != before {
		t.Errorf("a malformed frame changed the snapshot: %+v", after)
	}
}

func TestWallpaperSaveRejectsAndDedups(t *testing.T) {
	dir := t.TempDir()
	obs := newFrontendObserver(dir)

	// Rejected before anything is written.
	small := base64.StdEncoding.EncodeToString(testWallpaperPNG(t, 16, 16))
	if err := obs.saveWallpaper(small, ""); !errors.Is(err, errInvalidWallpaper) {
		t.Errorf("undersized wallpaper error = %v, want %v", err, errInvalidWallpaper)
	}
	if err := obs.saveWallpaper("not base64 @@", ""); err == nil {
		t.Error("invalid base64 must be rejected")
	}
	snap := obs.Snapshot()
	if snap.WallpaperBytes != 0 || snap.WallpaperVersion != 0 || snap.Version != 0 {
		t.Errorf("rejected wallpapers must not publish: %+v", snap)
	}
	if _, err := os.Stat(snap.WallpaperPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wallpaper file exists after a rejected push: %v", err)
	}

	raw := testWallpaperPNG(t, 64, 64)
	encoded := base64.StdEncoding.EncodeToString(raw)
	if err := obs.saveWallpaper(encoded, ""); err != nil {
		t.Fatalf("save accepted wallpaper: %v", err)
	}
	snap = obs.Snapshot()
	if snap.WallpaperBytes != len(raw) || snap.WallpaperWidth != 64 || snap.WallpaperHeight != 64 {
		t.Errorf("saved wallpaper = %+v", snap)
	}
	if snap.WallpaperVersion != 1 || snap.Version != 1 {
		t.Errorf("version = wallpaper %d / snapshot %d, want 1/1", snap.WallpaperVersion, snap.Version)
	}
	info, err := os.Stat(snap.WallpaperPath)
	if err != nil {
		t.Fatalf("wallpaper file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wallpaper perms = %v, want 0600", perm)
	}

	// The advertised md5 only fills an empty field; the bytes are hashed
	// locally, so a stale digest cannot reject or duplicate a wallpapers.
	if err := obs.saveWallpaper(encoded, strings.Repeat("0", 32)); err != nil {
		t.Errorf("a wrong md5 must not fail the save: %v", err)
	}
	if err := obs.saveWallpaper(encoded, md5Hex(raw)); err != nil {
		t.Errorf("the correct md5 must not fail the save: %v", err)
	}

	// Re-sending identical bytes is a no-op: no rewrite, no version bump.
	if err := obs.saveWallpaper(encoded, ""); err != nil {
		t.Fatalf("duplicate wallpaper: %v", err)
	}
	dedup := obs.Snapshot()
	if dedup.WallpaperVersion != snap.WallpaperVersion || dedup.Version != snap.Version ||
		dedup.WallpaperBytes != snap.WallpaperBytes {
		t.Errorf("duplicate wallpaper changed the snapshot: %+v", dedup)
	}
	rewritten, err := os.Stat(snap.WallpaperPath)
	if err != nil {
		t.Fatalf("wallpaper file: %v", err)
	}
	if !rewritten.ModTime().Equal(info.ModTime()) {
		t.Error("identical bytes were written again")
	}

	// New bytes are a new version.
	other := testWallpaperPNG(t, 96, 64)
	if err := obs.saveWallpaper(base64.StdEncoding.EncodeToString(other), ""); err != nil {
		t.Fatalf("changed wallpaper: %v", err)
	}
	fresh := obs.Snapshot()
	if fresh.WallpaperVersion != dedup.WallpaperVersion+1 || fresh.Version != dedup.Version+1 {
		t.Errorf("new bytes version = wallpaper %d / snapshot %d, want %d/%d",
			fresh.WallpaperVersion, fresh.Version, dedup.WallpaperVersion+1, dedup.Version+1)
	}
	if fresh.WallpaperBytes != len(other) || fresh.WallpaperWidth != 96 || fresh.WallpaperHeight != 64 {
		t.Errorf("new wallpaper = %+v", fresh)
	}
}

func TestObserverIgnoresForeignFramesAndNilReceiver(t *testing.T) {
	previous := frontendObserverHook
	t.Cleanup(func() { frontendObserverHook = previous })

	// No hook installed: nothing is decoded and nothing panics.
	frontendObserverHook = nil
	observeFrontendFrame(25, statusFrame(t, map[string]any{"model": "unused"}))

	dir := t.TempDir()
	obs := newFrontendObserver(dir)
	frontendObserverHook = obs.Observe
	raw := testWallpaperPNG(t, 64, 64)
	observeFrontendFrame(25, wallpaperFrame(t, base64.StdEncoding.EncodeToString(raw), md5Hex(raw)))
	snap := waitForSnapshot(t, obs, 2*time.Second, func(s frontendSnapshot) bool { return s.WallpaperBytes > 0 })
	if snap.WallpaperBytes != len(raw) {
		t.Errorf("the installed hook did not publish the wallpaper: %+v", snap)
	}

	// A nil observer (no UI, no control API) swallows frames instead of crashing.
	var absent *frontendObserver
	absent.Observe(25, wallpaperFrame(t, base64.StdEncoding.EncodeToString(raw), md5Hex(raw)))
	absent.Observe(0, []byte(`{}`))
}
