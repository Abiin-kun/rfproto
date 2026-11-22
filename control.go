// control.go: private HTTP control API for rfproto.
//
// Started by `serve` on a per-state UNIX socket by default; -ctl can select
// an explicit TCP address. Exposes the live viewer hub and frontend so
// `rfproto ctl` can query state, launch apps, stop sessions, and adjust
// orientation/focus/mute without re-pairing.
//
// Endpoints:
//
//	GET  /state        full viewer state (apps, sessions, audio, connection)
//	GET  /apps         app catalog list
//	POST /launch       launch app or desktop {op, mode, appId, width, height}
//	POST /stop         stop a stream session {sessionId}
//	POST /orientation  set stream orientation {sessionId, orientation}
//	POST /focus        raise/focus a stream window {sessionId}
//	POST /mute         set audio mute {mute}
//	GET  /wallpaper    wallpaper and phone status snapshot
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// controlFrontend is set by runNativeViewer so the control API can address
// stream windows (focus, mute). Nil when the UI is not running.
var controlFrontend *frontend
var controlFrontendMu sync.Mutex

var errFrontendSession = errors.New("stream window not found")

func controlRequest(w http.ResponseWriter, r *http.Request, f *frontend, req frontendRequest) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := f.request(ctx, req); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errFrontendSession) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

func setControlFrontend(f *frontend) {
	controlFrontendMu.Lock()
	defer controlFrontendMu.Unlock()
	controlFrontend = f
}

func getControlFrontend() *frontend {
	controlFrontendMu.Lock()
	defer controlFrontendMu.Unlock()
	return controlFrontend
}

type controlServer struct {
	server     *http.Server
	listener   net.Listener
	socketPath string
	socketInfo os.FileInfo
}

// startControlServer binds synchronously, after the caller acquires the state
// writer lock. Empty addr disables the API. Close before releasing that lock.
func startControlServer(addr string, store *peerStore) (*controlServer, error) {
	if addr == "" {
		return nil, nil
	}
	network, target, err := controlTarget(addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if store == nil || store.lock == nil {
			return nil, errors.New("control socket requires the state-directory writer lock")
		}
		if err := recoverControlSocket(target); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen(network, target)
	if err != nil {
		return nil, fmt.Errorf("bind control API %q: %w", addr, err)
	}
	s := &controlServer{listener: listener}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		// Never let net.UnixListener unlink a replacement socket on Close.
		unixListener.SetUnlinkOnClose(false)
		s.socketPath = target
		s.socketInfo, err = os.Lstat(target)
		if err == nil {
			err = os.Chmod(target, 0o600)
		}
		if err != nil {
			_ = listener.Close()
			s.removeSocket()
			return nil, fmt.Errorf("protect control socket: %w", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/state", handleState)
	mux.HandleFunc("/apps", handleApps)
	mux.HandleFunc("/launch", handleLaunch)
	mux.HandleFunc("/stop", handleStop)
	mux.HandleFunc("/orientation", handleOrientation)
	mux.HandleFunc("/focus", handleFocus)
	mux.HandleFunc("/mute", handleMute)
	mux.HandleFunc("/wallpaper", handleWallpaper)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[ctl] %v", err)
		}
	}()
	log.Printf("[ctl] control API on %s", addr)
	return s, nil
}

func (s *controlServer) Close() error {
	if s == nil {
		return nil
	}
	err := s.server.Close()
	_ = s.listener.Close()
	s.removeSocket()
	return err
}

func (s *controlServer) removeSocket() {
	if s.socketInfo == nil {
		return
	}
	info, err := os.Lstat(s.socketPath)
	if err == nil && os.SameFile(info, s.socketInfo) {
		_ = os.Remove(s.socketPath)
	}
}

// Only ECONNREFUSED proves that an existing socket has no listening owner.
// Permission errors, timeouts, regular files and symlinks are never removed.
func recoverControlSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %q is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("control socket %q is already active", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check control socket %q: %w", path, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("control socket %q changed during startup", path)
	}
	return os.Remove(path)
}

func defaultControlAddr(dir string) (string, error) {
	stateDir, err := resolveStateDir(dir)
	if err != nil {
		return "", err
	}
	return "unix:" + filepath.Join(stateDir, "control.sock"), nil
}

func controlTarget(addr string) (network, target string, err error) {
	if strings.HasPrefix(addr, "unix:") {
		path := strings.TrimPrefix(addr, "unix:")
		if !filepath.IsAbs(path) {
			return "", "", errors.New("control UNIX socket path must be absolute")
		}
		return "unix", path, nil
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", "", fmt.Errorf("invalid control address %q: use host:port or unix:/absolute/path: %w", addr, err)
	}
	return "tcp", addr, nil
}

// newControlClient shares address handling and a finite timeout across ctl/TUI.
// UNIX requests use a stable synthetic HTTP authority, never a TCP fallback.
func newControlClient(addr string) (*http.Client, string, error) {
	network, target, err := controlTarget(addr)
	if err != nil {
		return nil, "", err
	}
	transport := &http.Transport{}
	base := "http://" + target
	if network == "unix" {
		base = "http://rfproto-control"
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", target)
		}
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}, base, nil
}

func controlURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

func hub() *viewerHub { return streams }

// ---------- GET /state ----------

func handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	h := hub()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workspace not started"})
		return
	}
	muted := false
	if f := getControlFrontend(); f != nil {
		muted = f.muted.Load()
	}
	writeJSON(w, http.StatusOK, struct {
		viewerState
		Muted bool `json:"muted"`
	}{h.state(), muted})
}

// ---------- GET /apps ----------

func handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	h := hub()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workspace not started"})
		return
	}
	state := h.state()
	writeJSON(w, http.StatusOK, map[string]any{"apps": state.Apps, "limit": state.AppLimit})
}

// ---------- POST /launch ----------

func handleLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var cmd viewerCommand
	if err := json.Unmarshal(body, &cmd); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if cmd.Op != "" && cmd.Op != "launch" {
		http.Error(w, "unsupported op for /launch", http.StatusBadRequest)
		return
	}
	if cmd.Op == "" {
		cmd.Op = "launch"
	}
	h := hub()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workspace not started"})
		return
	}
	res, err := h.command(cmd)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- POST /stop ----------

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var cmd viewerCommand
	if err := json.Unmarshal(body, &cmd); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if cmd.Op != "" && cmd.Op != "stop" {
		http.Error(w, "unsupported op for /stop", http.StatusBadRequest)
		return
	}
	if cmd.Op == "" {
		cmd.Op = "stop"
	}
	h := hub()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workspace not started"})
		return
	}
	if _, err := h.command(cmd); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// ---------- POST /orientation ----------

func handleOrientation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID   string `json:"sessionId"`
		Orientation string `json:"orientation"` // auto, landscape, portrait
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "sessionId required", http.StatusBadRequest)
		return
	}
	orientation := strings.ToLower(strings.TrimSpace(req.Orientation))
	if orientation == "" {
		orientation = "auto"
	}
	switch orientation {
	case "auto", "landscape", "portrait":
	default:
		http.Error(w, "orientation must be auto, landscape, or portrait", http.StatusBadRequest)
		return
	}
	h := hub()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "workspace not started"})
		return
	}
	if _, err := h.command(viewerCommand{Op: "orientation", SessionID: req.SessionID, Orientation: orientation}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "orientation requested", "sessionId": req.SessionID, "orientation": orientation})
}

// ---------- POST /focus ----------

func handleFocus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "sessionId required", http.StatusBadRequest)
		return
	}
	f := getControlFrontend()
	if f == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "frontend not running"})
		return
	}
	if !controlRequest(w, r, f, frontendRequest{op: "focus", session: req.SessionID}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "focused", "sessionId": req.SessionID})
}

// ---------- POST /mute ----------

func handleMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Mute bool `json:"mute"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := getControlFrontend()
	if f == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "media not running"})
		return
	}
	if !controlRequest(w, r, f, frontendRequest{op: "mute", mute: req.Mute}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "mute set", "mute": strconv.FormatBool(req.Mute)})
}

// ---------- GET /wallpaper ----------

func handleWallpaper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	var snap frontendSnapshot
	if frontendAssets != nil {
		snap = frontendAssets.Snapshot()
	} else {
		snap = frontendSnapshot{Phone: phoneProfile{}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"phone":            snap.Phone,
		"wallpaperPath":    snap.WallpaperPath,
		"wallpaperVersion": snap.WallpaperVersion,
		"wallpaperBytes":   snap.WallpaperBytes,
		"wallpaperWidth":   snap.WallpaperWidth,
		"wallpaperHeight":  snap.WallpaperHeight,
		"version":          snap.Version,
	})
}

// ---------- ctl command (scriptable client) ----------

// ctlCommand runs the scriptable client, or opens the TUI on a TTY.
func ctlCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON output")
	addr := fs.String("addr", "", "control address: host:port or unix:/absolute/path")
	dir := fs.String("dir", "", "server state directory (default: XDG state directory)")
	positional, err := parseControlFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(positional) == 0 && !isTTY(stdout) {
		fmt.Fprintln(stdout, "rfproto ctl: control API client")
		fmt.Fprintln(stdout, "  ctl state          show workspace state")
		fmt.Fprintln(stdout, "  ctl apps           list shared apps")
		fmt.Fprintln(stdout, "  ctl launch app <appId>    launch an app using its catalog ID")
		fmt.Fprintln(stdout, "  ctl launch desktop       open the mobile desktop stream")
		fmt.Fprintln(stdout, "  ctl desktop        open the mobile desktop stream")
		fmt.Fprintln(stdout, "  ctl stop <sessionId>       stop a stream session")
		fmt.Fprintln(stdout, "  ctl orientation <sessionId> <auto|landscape|portrait>")
		fmt.Fprintln(stdout, "  ctl focus <sessionId>      raise a stream window")
		fmt.Fprintln(stdout, "  ctl mute <true|false>      set audio mute")
		fmt.Fprintln(stdout, "  ctl wallpaper          show wallpaper/phone status")
		fmt.Fprintln(stdout, "Flags (before or after command): -json, -addr, -dir")
		return nil
	}
	var path string
	var body any
	if len(positional) > 0 {
		command, values := positional[0], positional[1:]
		arity := func(n int, usage string) error {
			if len(values) != n {
				return fmt.Errorf("usage: rfproto ctl %s", usage)
			}
			for _, value := range values {
				if value == "" {
					return fmt.Errorf("usage: rfproto ctl %s (arguments must not be empty)", usage)
				}
			}
			return nil
		}
		switch command {
		case "state", "apps", "wallpaper", "desktop":
			if err := arity(0, command); err != nil {
				return err
			}
			path = "/" + command
			if command == "desktop" {
				path = "/launch"
				body = map[string]any{"op": "launch", "mode": "desktop", "width": 1280, "height": 720}
			}
		case "launch":
			if len(values) == 0 {
				return errors.New("usage: rfproto ctl launch app <appId> | launch desktop")
			}
			switch values[0] {
			case "app":
				if err := arity(2, "launch app <appId>"); err != nil {
					return err
				}
				body = map[string]any{"op": "launch", "mode": "app", "appId": values[1], "width": 1280, "height": 720}
			case "desktop":
				if err := arity(1, "launch desktop"); err != nil {
					return err
				}
				body = map[string]any{"op": "launch", "mode": "desktop", "width": 1280, "height": 720}
			default:
				return errors.New("launch mode must be app or desktop")
			}
			path = "/launch"
		case "stop", "focus":
			if err := arity(1, command+" <sessionId>"); err != nil {
				return err
			}
			path = "/" + command
			body = map[string]any{"sessionId": values[0]}
		case "orientation":
			if err := arity(2, "orientation <sessionId> <auto|landscape|portrait>"); err != nil {
				return err
			}
			orientation := strings.ToLower(values[1])
			switch orientation {
			case "auto", "landscape", "portrait":
			default:
				return errors.New("orientation must be auto, landscape, or portrait")
			}
			path = "/orientation"
			body = map[string]any{"sessionId": values[0], "orientation": orientation}
		case "mute":
			if err := arity(1, "mute <true|false>"); err != nil {
				return err
			}
			mute, err := strconv.ParseBool(values[0])
			if err != nil {
				return fmt.Errorf("mute must be a boolean: %w", err)
			}
			path, body = "/mute", map[string]any{"mute": mute}
		default:
			return fmt.Errorf("unknown ctl command %q (try: state, apps, launch, desktop, stop, orientation, focus, mute, wallpaper)", command)
		}
	}
	addrExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrExplicit = true
		}
	})
	if !addrExplicit {
		*addr, err = defaultControlAddr(*dir)
		if err != nil {
			return err
		}
	}
	if len(positional) == 0 {
		return ctlTUI(stdout, stderr, *addr)
	}
	client, base, err := newControlClient(*addr)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if body == nil {
		return ctlGet(client, base+path, *jsonOut, stdout)
	}
	return ctlPost(client, base+path, body, *jsonOut, stdout)
}

// flag.FlagSet stops at the first positional argument. Collect flags first so
// the same documented flags work on either side of the command; honor --.
func parseControlFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil || hasValue {
			continue
		}
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return positional, fs.Parse(flags)
}

func ctlGet(client *http.Client, url string, jsonOut bool, stdout io.Writer) error {
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, strings.TrimSpace(string(body)))
	}
	return ctlOutput(stdout, body, jsonOut)
}

func ctlPost(client *http.Client, url string, body any, jsonOut bool, stdout io.Writer) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s", url, strings.TrimSpace(string(out)))
	}
	return ctlOutput(stdout, out, jsonOut)
}

func ctlOutput(stdout io.Writer, body []byte, jsonOut bool) error {
	if jsonOut {
		body = compactJSON(body)
	}
	if _, err := stdout.Write(body); err != nil {
		return err
	}
	if jsonOut {
		_, err := io.WriteString(stdout, "\n")
		return err
	}
	return nil
}

func compactJSON(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func isTTY(w io.Writer) bool {
	// Best-effort: only os.Stdout/os.Stderr can be TTYs.
	switch f := w.(type) {
	case *os.File:
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

// ctlTUI runs the terminal UI against the control API at addr.
func ctlTUI(stdout, stderr io.Writer, addr string) error {
	return runTUI(addr, stdout, stderr)
}
