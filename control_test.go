// control_test.go: HTTP-surface and private transport tests for the control API.
//
// Handler tests use httptest recorders; transport tests bind real ephemeral
// TCP listeners and private temporary UNIX sockets. Package-level handler
// dependencies (streams, controlFrontend, frontendAssets) are saved and
// restored per test so other tests keep their own view of the process.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// controlCall drives one control handler the way the mux does.
func controlCall(t *testing.T, handler http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/control", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func controlJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return out
}

// withStreams installs a test-owned viewer hub and restores the previous one.
func withStreams(t *testing.T, h *viewerHub) {
	t.Helper()
	previous := streams
	streams = h
	t.Cleanup(func() { streams = previous })
}

// withFrontend installs a leading native frontend and restores the previous one.
func withFrontend(t *testing.T, f *frontend) {
	t.Helper()
	previous := getControlFrontend()
	setControlFrontend(f)
	if f != nil {
		f.requests = make(chan frontendRequest)
		f.done = make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			for {
				select {
				case req := <-f.requests:
					f.handleRequest(req)
				case <-f.done:
					return
				}
			}
		}()
		t.Cleanup(func() { close(f.done); <-stopped })
	}
	t.Cleanup(func() { setControlFrontend(previous) })
}

// withFrontendAssets installs an observer and restores the previous one.
func withFrontendAssets(t *testing.T, assets *frontendObserver) {
	t.Helper()
	previous := frontendAssets
	frontendAssets = assets
	t.Cleanup(func() { frontendAssets = previous })
}

// startedHub is a hub with an open phone workspace subscription but no phone,
// so commands stop at the first connection check.
func startedHub(t *testing.T) *viewerHub {
	t.Helper()
	h := newViewerHub()
	t.Cleanup(h.close)
	_, cancel, err := h.subscribe()
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(cancel)
	return h
}

// pairedHub is a hub with an open workspace and a paired phone whose replies
// are discarded, so command validation runs after the connection checks.
func pairedHub(t *testing.T) *viewerHub {
	t.Helper()
	h := newViewerHub()
	t.Cleanup(h.close)
	h.controlConnected(&liveSession{name: "Test phone", send: func(_ string, _ []byte) error { return nil }})
	_, cancel, err := h.subscribe()
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(cancel)
	return h
}

func TestControlHandlersRequireStartedHub(t *testing.T) {
	previous := streams
	streams = nil
	t.Cleanup(func() { streams = previous })

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    string
	}{
		{"state", handleState, http.MethodGet, ""},
		{"apps", handleApps, http.MethodGet, ""},
		{"launch", handleLaunch, http.MethodPost, `{"op":"launch","mode":"desktop","width":1280,"height":720}`},
		{"stop", handleStop, http.MethodPost, `{"op":"stop","sessionId":"v1"}`},
		{"orientation", handleOrientation, http.MethodPost, `{"sessionId":"v1","orientation":"portrait"}`},
	}
	for _, tc := range cases {
		rec := controlCall(t, tc.handler, tc.method, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, http.StatusServiceUnavailable, rec.Body.String())
		}
		body := controlJSON(t, rec)
		if body["error"] != "workspace not started" {
			t.Errorf("%s: error %q, want %q", tc.name, body["error"], "workspace not started")
		}
	}
}

func TestControlLaunchValidation(t *testing.T) {
	t.Run("rejects commands for another endpoint", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			handler http.HandlerFunc
			body    string
			want    string
		}{
			{"launch rejects input", handleLaunch, `{"op":"input","type":"touch","sessionId":"v1"}`, "unsupported op for /launch"},
			{"stop rejects input", handleStop, `{"op":"input","type":"touch","sessionId":"v1"}`, "unsupported op for /stop"},
		} {
			rec := controlCall(t, tc.handler, http.MethodPost, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want %d", tc.name, rec.Code, http.StatusBadRequest)
			}
			if got := rec.Body.String(); !strings.Contains(got, tc.want) {
				t.Errorf("%s: body %q does not contain %q", tc.name, got, tc.want)
			}
		}
	})
	// The connection checks come first: with an open workspace but no phone,
	// every launch is refused before its size, mode or op is inspected.
	t.Run("workspace before validation", func(t *testing.T) {
		withStreams(t, startedHub(t))
		cases := []struct {
			name string
			body string
		}{
			{"desktop 720p", `{"op":"launch","mode":"desktop","width":1280,"height":720}`},
			{"desktop 1080p", `{"op":"launch","mode":"desktop","width":1920,"height":1080}`},
			{"unsupported size", `{"op":"launch","mode":"desktop","width":800,"height":600}`},
			{"unknown mode", `{"op":"launch","mode":"handheld","width":1280,"height":720}`},
			{"default op is launch", `{"mode":"desktop","width":1280,"height":720}`},
		}
		for _, tc := range cases {
			rec := controlCall(t, handleLaunch, http.MethodPost, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			body := controlJSON(t, rec)
			if body["error"] != "connect the paired phone first" {
				t.Errorf("%s: error %q, want %q", tc.name, body["error"], "connect the paired phone first")
			}
		}
	})

	// With a phone attached the workspace rejects the command itself.
	t.Run("paired phone validates the command", func(t *testing.T) {
		withStreams(t, pairedHub(t))
		cases := []struct {
			name    string
			body    string
			wantErr string
		}{
			{"unsupported size", `{"op":"launch","mode":"desktop","width":800,"height":600}`, "choose a supported display size"},
			{"unknown mode", `{"op":"launch","mode":"handheld","width":1280,"height":720}`, "unknown streaming mode"},
			{"missing app", `{"op":"launch","mode":"app","appId":"gone/nowhere:0:","width":1280,"height":720}`, "app is no longer in the phone's catalog"},
			{"unknown op", `{"op":"hibernate","width":1280,"height":720}`, "unsupported op for /launch"},
		}
		for _, tc := range cases {
			rec := controlCall(t, handleLaunch, http.MethodPost, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if tc.wantErr == "unsupported op for /launch" {
				if got := strings.TrimSpace(rec.Body.String()); got != tc.wantErr {
					t.Errorf("%s: error %q, want %q", tc.name, got, tc.wantErr)
				}
				continue
			}
			body := controlJSON(t, rec)
			if body["error"] != tc.wantErr {
				t.Errorf("%s: error %q, want %q", tc.name, body["error"], tc.wantErr)
			}
		}
	})

	t.Run("closed workspace", func(t *testing.T) {
		// A hub nobody subscribed to: the workspace itself is not open.
		idle := newViewerHub()
		t.Cleanup(idle.close)
		withStreams(t, idle)
		rec := controlCall(t, handleLaunch, http.MethodPost, `{"op":"launch","mode":"desktop","width":1280,"height":720}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want %d (%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		if body := controlJSON(t, rec); body["error"] != "phone workspace is not connected" {
			t.Errorf("error %q, want %q", body["error"], "phone workspace is not connected")
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		rec := controlCall(t, handleLaunch, http.MethodPost, `{"op":`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

func TestControlStateAndAppsWithHub(t *testing.T) {
	withStreams(t, pairedHub(t))

	rec := controlCall(t, handleState, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("state: status %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("state content type %q, want JSON", ct)
	}
	state := controlJSON(t, rec)
	for _, key := range []string{"connected", "phone", "apps", "appLimit", "sessions", "audio"} {
		if _, ok := state[key]; !ok {
			t.Errorf("state is missing %q: %v", key, state)
		}
	}
	if state["connected"] != true || state["phone"] != "Test phone" {
		t.Errorf("connection = %v %q, want true %q", state["connected"], state["phone"], "Test phone")
	}

	rec = controlCall(t, handleApps, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("apps: status %d (%s)", rec.Code, rec.Body.String())
	}
	apps := controlJSON(t, rec)
	if apps["limit"] != float64(0) {
		t.Errorf("apps limit = %v, want 0", apps["limit"])
	}
	if _, ok := apps["apps"]; !ok {
		t.Errorf("apps is missing the catalog: %v", apps)
	}
}

func TestControlLaunchAndStopLifecycle(t *testing.T) {
	h := pairedHub(t)
	withStreams(t, h)

	// A desktop launch at a supported size answers with the session it created.
	rec := controlCall(t, handleLaunch, http.MethodPost, `{"op":"launch","mode":"desktop","width":1280,"height":720}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("launch: status %d (%s)", rec.Code, rec.Body.String())
	}
	body := controlJSON(t, rec)
	sessionID, _ := body["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("launch returned no session: %v", body)
	}

	state := h.state()
	if !state.Connected || state.Phone != "Test phone" {
		t.Errorf("connection = %v %q, want the paired phone", state.Connected, state.Phone)
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one", state.Sessions)
	}
	session := state.Sessions[0]
	if session.ID != sessionID || session.Mode != "desktop" || session.Name != "Mobile desktop" ||
		session.Width != 1280 || session.Height != 720 || session.State != "requesting" {
		t.Errorf("session = %+v, want the requested desktop", session)
	}

	// Launching the desktop again reuses the live session.
	rec = controlCall(t, handleLaunch, http.MethodPost, `{"op":"launch","mode":"desktop","width":1280,"height":720}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-launch: status %d (%s)", rec.Code, rec.Body.String())
	}
	if reuse := controlJSON(t, rec); reuse["sessionId"] != sessionID {
		t.Errorf("re-launch reused %v, want %q", reuse["sessionId"], sessionID)
	}

	// Stopping that session removes it from the published state.
	rec = controlCall(t, handleStop, http.MethodPost, `{"op":"stop","sessionId":"`+sessionID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: status %d (%s)", rec.Code, rec.Body.String())
	}
	body = controlJSON(t, rec)
	if body["status"] != "stopped" {
		t.Errorf("stop status = %v, want %q", body["status"], "stopped")
	}
	if state := h.state(); len(state.Sessions) != 0 {
		t.Errorf("stop left %d sessions: %+v", len(state.Sessions), state.Sessions)
	}

	// The op defaults to stop, and an unknown session is still acknowledged.
	rec = controlCall(t, handleStop, http.MethodPost, `{"sessionId":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("default op: status %d (%s)", rec.Code, rec.Body.String())
	}
	body = controlJSON(t, rec)
	if body["status"] != "stopped" {
		t.Errorf("default op status = %v, want %q", body["status"], "stopped")
	}
}

func TestControlMuteAndFocusRequireFrontend(t *testing.T) {
	withFrontend(t, nil)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		body    string
		wantErr string
	}{
		{"mute on", handleMute, `{"mute":true}`, "media not running"},
		{"mute off", handleMute, `{"mute":false}`, "media not running"},
		{"focus", handleFocus, `{"sessionId":"v1"}`, "frontend not running"},
	}
	for _, tc := range cases {
		rec := controlCall(t, tc.handler, http.MethodPost, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, http.StatusServiceUnavailable, rec.Body.String())
		}
		body := controlJSON(t, rec)
		if body["error"] != tc.wantErr {
			t.Errorf("%s: error %q, want %q", tc.name, body["error"], tc.wantErr)
		}
	}

	// Body validation is reported before the missing frontend is noticed.
	rejected := []struct {
		name    string
		handler http.HandlerFunc
		body    string
		want    string
	}{
		{"orientation without sessionId", handleOrientation, `{"orientation":"portrait"}`, "sessionId required"},
		{"unknown orientation", handleOrientation, `{"sessionId":"v1","orientation":"upside down"}`, "orientation must be auto, landscape, or portrait"},
		{"focus without sessionId", handleFocus, `{}`, "sessionId required"},
	}
	for _, tc := range rejected {
		rec := controlCall(t, tc.handler, http.MethodPost, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, http.StatusBadRequest)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
			t.Errorf("%s: body %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestControlFrontendHandlersWithoutUI(t *testing.T) {
	// HTTP commands cross the frontend queue. Unknown windows must not
	// report success; this test owner executes requests without invoking SDL.
	front := &frontend{media: &nativeMedia{}}
	withFrontend(t, front)

	rec := controlCall(t, handleMute, http.MethodPost, `{"mute":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mute: status %d (%s)", rec.Code, rec.Body.String())
	}
	body := controlJSON(t, rec)
	if body["status"] != "mute set" || body["mute"] != "true" {
		t.Errorf("mute response %v", body)
	}
	if !front.media.muted {
		t.Error("mute was not forwarded to the media session")
	}

	rec = controlCall(t, handleMute, http.MethodPost, `{"mute":false}`)
	if rec.Code != http.StatusOK || front.media.muted {
		t.Errorf("unmute: status %d muted=%t", rec.Code, front.media.muted)
	}

	rec = controlCall(t, handleFocus, http.MethodPost, `{"sessionId":"v1"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown focus: status %d (%s)", rec.Code, rec.Body.String())
	}

	t.Run("missing media", func(t *testing.T) {
		withFrontend(t, &frontend{})
		rec := controlCall(t, handleMute, http.MethodPost, `{"mute":true}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		if body := controlJSON(t, rec); body["error"] != "media not running" {
			t.Errorf("error %q, want %q", body["error"], "media not running")
		}
	})
}

// wallpaperReply mirrors the GET /wallpaper response, so the assertions stay
// typed instead of comparing loosely typed JSON numbers.
type wallpaperReply struct {
	Phone            phoneProfile `json:"phone"`
	WallpaperPath    string       `json:"wallpaperPath"`
	WallpaperVersion uint64       `json:"wallpaperVersion"`
	WallpaperBytes   int          `json:"wallpaperBytes"`
	WallpaperWidth   int          `json:"wallpaperWidth"`
	WallpaperHeight  int          `json:"wallpaperHeight"`
	Version          uint64       `json:"version"`
}

func wallpaperGet(t *testing.T) wallpaperReply {
	t.Helper()
	rec := controlCall(t, handleWallpaper, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wallpaper: status %d (%s)", rec.Code, rec.Body.String())
	}
	var reply wallpaperReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("wallpaper response is not JSON: %v", err)
	}
	return reply
}

func TestControlWallpaperEndpoint(t *testing.T) {
	dir := t.TempDir()

	// Without an observer the endpoint still answers, with an empty snapshot.
	withFrontendAssets(t, nil)
	empty := wallpaperGet(t)
	if empty.Phone != (phoneProfile{}) || empty.WallpaperPath != "" ||
		empty.WallpaperBytes != 0 || empty.WallpaperVersion != 0 || empty.Version != 0 {
		t.Errorf("empty snapshot: %+v", empty)
	}

	assets := newFrontendObserver(dir)
	withFrontendAssets(t, assets)
	raw := testWallpaperPNG(t, 64, 64)
	assets.Observe(25, wallpaperFrame(t, base64.StdEncoding.EncodeToString(raw), md5Hex(raw)))
	snap := waitForSnapshot(t, assets, 2*time.Second, func(s frontendSnapshot) bool { return s.WallpaperBytes > 0 })

	got := wallpaperGet(t)
	want := wallpaperReply{
		Phone:            phoneProfile{},
		WallpaperPath:    filepath.Join(dir, "wallpaper.png"),
		WallpaperVersion: snap.WallpaperVersion,
		WallpaperBytes:   len(raw),
		WallpaperWidth:   64,
		WallpaperHeight:  64,
		Version:          snap.Version,
	}
	if got != want {
		t.Errorf("wallpaper = %+v, want %+v", got, want)
	}
}

func TestControlMethodGuards(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		want    string
	}{
		{"state", handleState, http.MethodPost, "GET only"},
		{"apps", handleApps, http.MethodPost, "GET only"},
		{"wallpaper", handleWallpaper, http.MethodPost, "GET only"},
		{"launch", handleLaunch, http.MethodGet, "POST only"},
		{"stop", handleStop, http.MethodGet, "POST only"},
		{"orientation", handleOrientation, http.MethodGet, "POST only"},
		{"focus", handleFocus, http.MethodGet, "POST only"},
		{"mute", handleMute, http.MethodGet, "POST only"},
	}
	for _, tc := range cases {
		rec := controlCall(t, tc.handler, tc.method, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.name, rec.Code, http.StatusMethodNotAllowed)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
			t.Errorf("%s %s: body %q, want %q", tc.method, tc.name, got, tc.want)
		}
	}
}

// TestControlServerRoundTrip exercises real TCP binding and the shared client.
func TestControlServerRoundTrip(t *testing.T) {
	server, err := startControlServer("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, base, err := newControlClient(server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	withStreams(t, nil)

	// No hub: every route reports the workspace is not started.
	for _, path := range []string{"/state", "/apps", "/wallpaper"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status %d (%s)", path, resp.StatusCode, body)
		}
	}

	h := startedHub(t)
	withStreams(t, h)
	resp, err := client.Get(base + "/state")
	if err != nil {
		t.Fatalf("GET /state: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /state: status %d (%s)", resp.StatusCode, body)
	}
	var state viewerState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if state.Connected {
		t.Error("state reports a phone that was never connected")
	}

	post, err := client.Post(base+"/launch", "application/json",
		strings.NewReader(`{"op":"launch","mode":"desktop","width":1280,"height":720}`))
	if err != nil {
		t.Fatalf("POST /launch: %v", err)
	}
	failure, _ := io.ReadAll(post.Body)
	post.Body.Close()
	if post.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /launch: status %d, want %d (%s)", post.StatusCode, http.StatusBadRequest, failure)
	}
	var launch map[string]string
	if err := json.Unmarshal(failure, &launch); err != nil {
		t.Fatalf("launch error is not JSON: %v", err)
	}
	if launch["error"] != "connect the paired phone first" {
		t.Errorf("launch error %q, want %q", launch["error"], "connect the paired phone first")
	}
}

func TestControlServerSkipsEmptyAddress(t *testing.T) {
	previous := streams
	streams = nil
	t.Cleanup(func() { streams = previous })

	// An empty address must disable the API instead of binding a wildcard port.
	server, err := startControlServer("", nil)
	if err != nil || server != nil {
		t.Fatalf("disabled server = %v, %v", server, err)
	}

	rec := controlCall(t, handleState, http.MethodGet, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestCompactJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already compact", `{"a":1,"b":[1,2]}`, `{"a":1,"b":[1,2]}`},
		{"whitespace removed", "{ \"a\" : 1 , \"b\" : [ 1 , 2 ] }", `{"a":1,"b":[1,2]}`},
		{"keys are sorted", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"invalid json is unchanged", `not json at all`, `not json at all`},
		{"truncated json is unchanged", `{"a":`, `{"a":`},
		{"empty is unchanged", ``, ``},
	}
	for _, tc := range cases {
		got := string(compactJSON([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("%s: compactJSON(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if tc.name == "whitespace removed" && strings.ContainsAny(got, " \t\r\n") {
			t.Errorf("%s: output still has whitespace: %q", tc.name, got)
		}
	}
}

func TestIsTTY(t *testing.T) {
	if isTTY(&bytes.Buffer{}) {
		t.Error("an in-memory buffer is not a terminal")
	}
	file, err := os.CreateTemp(t.TempDir(), "tty-*")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer file.Close()
	if isTTY(file) {
		t.Errorf("%s is a regular file, not a terminal", file.Name())
	}
	// `go test` captures os.Stdout, so it is normally a pipe; either answer is
	// valid, the call must only report a bool without panicking.
	t.Logf("os.Stdout is a terminal: %v", isTTY(os.Stdout))
}

func startTestControlSocket(t *testing.T, dir string) (*controlServer, string) {
	t.Helper()
	store, err := openPeerStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := defaultControlAddr(dir)
	if err != nil {
		_ = store.close()
		t.Fatal(err)
	}
	server, err := startControlServer(addr, store)
	if err != nil {
		_ = store.close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(); _ = store.close() })
	return server, addr
}

func TestControlDefaultSocketAndExplicitTCP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	_, addr := startTestControlSocket(t, filepath.Join(dir, "rfproto"))
	h := pairedHub(t)
	h.control.name = "Private state-directory phone"
	withStreams(t, h)
	withFrontend(t, nil)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state" {
			t.Errorf("TCP target = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"otherDaemon":true}`)
	}))
	defer tcp.Close()

	var out bytes.Buffer
	if err := ctlCommand([]string{"state", "-json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(out.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["otherDaemon"] != nil || state["phone"] != "Private state-directory phone" || state["muted"] != false || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("default command reached wrong state server: %s", out.String())
	}
	resolved, err := defaultControlAddr("")
	if err != nil || resolved != addr {
		t.Fatalf("default address = %q, %v; want %q", resolved, err, addr)
	}
	out.Reset()
	tcpAddr := strings.TrimPrefix(tcp.URL, "http://")
	if err := ctlCommand([]string{"-addr", tcpAddr, "state", "-json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"otherDaemon\":true}\n" {
		t.Fatalf("explicit TCP output = %q", out.String())
	}
	out.Reset()
	if err := ctlCommand([]string{"state", "-dir", filepath.Join(dir, "rfproto"), "-json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ctlCommand([]string{"state", "-addr", addr, "-json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestControlSocketLifecycleAndPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	server, addr := startTestControlSocket(t, dir)
	for path, want := range map[string]os.FileMode{dir: 0o700, strings.TrimPrefix(addr, "unix:"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions %s: %v, %v; want %o", path, info, err, want)
		}
	}
	client, base, err := newControlClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.Timeout <= 0 || base != "http://rfproto-control" {
		t.Fatalf("UNIX client has timeout %s and authority %q", client.Timeout, base)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(strings.TrimPrefix(addr, "unix:")); !os.IsNotExist(err) {
		t.Fatalf("shutdown left socket: %v", err)
	}
	if _, err := client.Get(base + "/state"); err == nil {
		t.Fatal("closed UNIX client unexpectedly reached another server")
	}
}

func TestControlSocketStaleRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	server, addr := startTestControlSocket(t, dir)
	withStreams(t, nil)
	client, base, err := newControlClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(base + "/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("recovered socket status = %d", resp.StatusCode)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlSocketRefusesForeignPaths(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "active"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store, err := openPeerStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			path := filepath.Join(dir, "control.sock")
			switch kind {
			case "regular":
				err = os.WriteFile(path, []byte("keep me"), 0o600)
			case "symlink":
				target := filepath.Join(dir, "keep")
				if err = os.WriteFile(target, []byte("keep me"), 0o600); err == nil {
					err = os.Symlink(target, path)
				}
			case "active":
				var listener *net.UnixListener
				listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err == nil {
					listener.SetUnlinkOnClose(false)
					defer listener.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if server, err := startControlServer("unix:"+path, store); err == nil {
				_ = server.Close()
				t.Fatal("startup accepted an occupied control path")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("startup replaced foreign path: %v", err)
			}
			if kind != "active" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "keep me" {
					t.Fatalf("foreign file changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestControlShutdownPreservesReplacementSocket(t *testing.T) {
	server, addr := startTestControlSocket(t, t.TempDir())
	path := strings.TrimPrefix(addr, "unix:")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		t.Fatalf("shutdown removed another listener's socket: %v", err)
	}
}

func TestControlStartupReportsBindingErrors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if server, err := startControlServer(listener.Addr().String(), nil); err == nil {
		_ = server.Close()
		t.Fatal("occupied TCP address was silently accepted")
	}
	path := filepath.Join(t.TempDir(), "control.sock")
	if server, err := startControlServer("unix:"+path, nil); err == nil {
		_ = server.Close()
		t.Fatal("UNIX listener started without the writer lock")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unlocked startup created a socket: %v", err)
	}
}

func TestControlArgumentErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-missing"}, {"-addr"}, {"state", "extra"}, {"apps", "extra"},
		{"wallpaper", "extra"}, {"desktop", "extra"}, {"launch"}, {"launch", "bad"},
		{"launch", "app"}, {"launch", "app", "id", "extra"}, {"launch", "desktop", "id"},
		{"stop"}, {"stop", "one", "two"}, {"focus"}, {"focus", ""},
		{"orientation", "session"}, {"orientation", "session", "bad"},
		{"mute"}, {"mute", "garbage"}, {"mute", "false", "extra"}, {"-json=garbage", "state"},
		{"state", "-addr", ""}, {"state", "-addr", "unix:relative.sock"},
		{"state", "-addr", "http://localhost:1234"}, {"unknown"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out, stderr bytes.Buffer
			if err := ctlCommand(args, &out, &stderr); err == nil {
				t.Fatalf("accepted invalid arguments %q; output %s", args, out.String())
			}
		})
	}
	if err := ctlCommand([]string{"-help"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestControlCLIRequestTargets(t *testing.T) {
	type request struct {
		Method string
		Path   string
		Body   map[string]any
	}
	requests := make(chan request, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := request{Method: r.Method, Path: r.URL.Path}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&got.Body); err != nil {
				t.Error(err)
			}
		}
		requests <- got
		if r.URL.Path == "/orientation" {
			w.WriteHeader(http.StatusAccepted)
		}
		fmt.Fprint(w, "{ \"ok\": true }\n")
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	appID := "com.example/com.example.Main:10:shortcut"
	cases := []struct {
		args []string
		want request
	}{
		{[]string{"state"}, request{Method: "GET", Path: "/state"}},
		{[]string{"apps"}, request{Method: "GET", Path: "/apps"}},
		{[]string{"wallpaper"}, request{Method: "GET", Path: "/wallpaper"}},
		{[]string{"launch", "app", appID}, request{Method: "POST", Path: "/launch", Body: map[string]any{"op": "launch", "mode": "app", "appId": appID, "width": float64(1280), "height": float64(720)}}},
		{[]string{"launch", "desktop"}, request{Method: "POST", Path: "/launch", Body: map[string]any{"op": "launch", "mode": "desktop", "width": float64(1280), "height": float64(720)}}},
		{[]string{"desktop"}, request{Method: "POST", Path: "/launch", Body: map[string]any{"op": "launch", "mode": "desktop", "width": float64(1280), "height": float64(720)}}},
		{[]string{"stop", "v2"}, request{Method: "POST", Path: "/stop", Body: map[string]any{"sessionId": "v2"}}},
		{[]string{"focus", "v2"}, request{Method: "POST", Path: "/focus", Body: map[string]any{"sessionId": "v2"}}},
		{[]string{"orientation", "v2", "LANDSCAPE"}, request{Method: "POST", Path: "/orientation", Body: map[string]any{"sessionId": "v2", "orientation": "landscape"}}},
		{[]string{"mute", "true"}, request{Method: "POST", Path: "/mute", Body: map[string]any{"mute": true}}},
		{[]string{"mute", "false"}, request{Method: "POST", Path: "/mute", Body: map[string]any{"mute": false}}},
	}
	for i, tc := range cases {
		args := append([]string{}, tc.args...)
		if i%2 == 0 {
			args = append([]string{"-addr", addr, "-json"}, args...)
		} else {
			args = append(args, "-json", "-addr", addr)
		}
		var out bytes.Buffer
		if err := ctlCommand(args, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := <-requests; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q sent %+v, want %+v", args, got, tc.want)
		}
		if out.String() != "{\"ok\":true}\n" {
			t.Fatalf("JSON output = %q", out.String())
		}
	}
}

func TestControlOrientationUsesRemoteHubWithoutFrontend(t *testing.T) {
	h, _, sent, _ := streamingFixture(t)
	withStreams(t, h)
	withFrontend(t, nil)
	session := h.newSessionLocked("app", catalogApp{Pkg: "example.clock"}, "Clock", 1280, 720)
	session.State, session.DisplayID, session.wireID = "connected", 3, 42
	session.Orientation = "auto"
	before := len(*sent)
	rec := controlCall(t, handleOrientation, http.MethodPost, `{"sessionId":"`+session.ID+`","orientation":" PORTRAIT "}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("remote orientation: %d %s", rec.Code, rec.Body.String())
	}
	if body := controlJSON(t, rec); body["orientation"] != "portrait" || body["sessionId"] != session.ID || body["status"] != "orientation requested" {
		t.Fatalf("orientation response = %v", body)
	}
	if len(*sent) != before+1 {
		t.Fatalf("remote orientation sent %d phone messages, want one", len(*sent)-before)
	}
	if session.Orientation != "auto" || session.OrientationPending != "portrait" {
		t.Fatalf("request was not pending phone confirmation: %+v", session.viewerSession)
	}
	withStreams(t, startedHub(t))
	rec = controlCall(t, handleOrientation, http.MethodPost, `{"sessionId":"v1","orientation":"portrait"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "connect the paired phone first") {
		t.Fatalf("remote command error not propagated: %d %s", rec.Code, rec.Body.String())
	}
}
