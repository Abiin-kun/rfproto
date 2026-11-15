// tui.go: terminal UI for rfproto.
//
// Uses golang.org/x/term (no framework). Runs on the alternate screen with a
// true-black / lime theme. Connects to the private per-state control socket
// started by `serve` (or an explicit -addr); if the API is unreachable it shows
// a connection error instead of crashing.
//
// Bare `rfproto ctl` on a TTY opens this TUI; `rfproto tui` is an alias.
// Non-TTY ctl prints plain output (see control.go).
//
// Layout: the app list on the left and the detail/open-display/phone/wallpaper
// panes on the right at >= 100 columns (~55%/45%), collapsing to a single
// stacked column below that. The wallpaper thumbnail is decoded in the
// background (paced by wallpaperVersion) and rendered as half-block pixels.
//
// Keys:
//
//	↑↓ / jk        move selection
//	PgUp/PgDn      scroll page
//	Home/End       jump to top/bottom
//	Enter          open selected app / desktop
//	d              open mobile desktop
//	/              focus search
//	Esc            clear search / close help
//	Tab            switch focus pane
//	o              cycle orientation (auto/landscape/portrait) on open display
//	x              stop selected session
//	m              toggle audio mute (last successful request)
//	r              refresh state
//	q              quit
//	?              help overlay
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// TUI colors (true-black ground, lime for live/data status only).
const (
	tuiGround       = "\x1b[48;5;232m" // true black background
	tuiSurface      = "\x1b[48;5;236m" // raised surface
	tuiText         = "\x1b[38;5;252m" // near-white text
	tuiMuted        = "\x1b[38;5;244m" // muted gray
	tuiDim          = "\x1b[38;5;240m" // dim gray
	tuiAccent       = "\x1b[38;5;118m" // lime #c6f24e-ish (256-color approx)
	tuiSelected     = "\x1b[48;5;234m" // #1a1a1a selection bg
	tuiSelectedText = "\x1b[38;5;252m"
	tuiBorder       = "\x1b[38;5;238m" // subtle border
	tuiReset        = "\x1b[0m"
	tuiBold         = "\x1b[1m"
	tuiHideCursor   = "\x1b[?25l"
	tuiShowCursor   = "\x1b[?25h"
	tuiAltScreen    = "\x1b[?1049h"
	tuiExitAlt      = "\x1b[?1049l"
	tuiClear        = "\x1b[2J"
	tuiHome         = "\x1b[H"
)

// Layout: at tuiTwoColMinWidth columns and wider the UI splits into an app
// list on the left (~55% of the width) and the detail, open-display, phone
// and wallpaper panes on the right (~45%). Narrower terminals collapse to the
// single stacked column with the right pane below the list.
const (
	tuiTwoColMinWidth = 100 // minimum width for two columns
	tuiGutter         = 2   // blank cells between the columns
	tuiThumbCells     = 24  // wallpaper thumbnail width (cells)
	tuiThumbMaxRows   = 16  // wallpaper thumbnail height cap (half-block rows)
	tuiWallpaperMinB  = 64  // smaller files are not real PNGs
)

// rgb is an 8-bit truecolor triple.
type rgb [3]uint8

// tuiPixel pairs the top and bottom pixel of one half-block cell: the render
// draws "▀" with the top pixel as the foreground color and the bottom pixel as
// the background color.
type tuiPixel struct {
	top    rgb
	bottom rgb
}

// wallpaperThumb caches the scaled wallpaper thumbnail together with the
// wallpaperVersion it was decoded from, so a 30fps render tick never decodes.
type wallpaperThumb struct {
	version uint64
	imgW    int
	imgH    int
	rows    [][]tuiPixel // one tuiPixel per terminal cell, per cell row
}

// tuiState mirrors viewer state fetched from the control API.
type tuiState struct {
	Connected bool            `json:"connected"`
	Phone     string          `json:"phone"`
	Apps      []viewerApp     `json:"apps"`
	AppLimit  int             `json:"appLimit"`
	Sessions  []viewerSession `json:"sessions"`
	Muted     bool            `json:"muted"`
	Audio     viewerAudio     `json:"audio"`
}

// tuiModel is owned by the event loop. Workers return immutable snapshots;
// they never read or write the model.
type tuiModel struct {
	addr            string
	client          *http.Client
	state           tuiState
	selected        int
	search          string
	searchFocus     bool
	scroll          int
	sessionsFocus   bool
	sessionSelected int
	sessionScroll   int
	sessionID       string
	help            bool
	helpScroll      int
	muted           bool
	muteRevision    uint64
	statusMsg       string
	statusErr       bool
	fetchErr        string
	quitting        bool
	fd              int
	width           int
	height          int
	wallpaper       *wallpaperThumb
	wallpaperBad    uint64
	lastFetch       time.Time
	fetching        bool
	results         chan tuiFetchResult
}

type tuiFetchResult struct {
	state        *tuiState
	err          error
	wallpaper    *wallpaperThumb
	wallpaperBad uint64
	muteRevision uint64
}

const tuiFetchInterval = 1 * time.Second

// runTUI enters the alternate screen and runs the event loop.
func runTUI(addr string, stdout, stderr io.Writer) error {
	file, ok := stdout.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("TUI requires a TTY")
	}
	client, base, err := newControlClient(addr)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	m := &tuiModel{
		addr: base, client: client,
		fd: int(file.Fd()), width: 80, height: 24,
		results: make(chan tuiFetchResult, 1),
	}
	m.updateSize()
	m.startFetch()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	fmt.Fprint(stdout, tuiAltScreen+tuiClear+tuiHome+tuiHideCursor)
	defer fmt.Fprint(stdout, tuiShowCursor+tuiExitAlt+tuiClear+tuiHome)
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	done := make(chan struct{})
	defer close(done)
	keys := readKeys(fd, done)
	m.render(stdout)
	for !m.quitting {
		select {
		case <-sigCh:
			m.quitting = true
		case result := <-m.results:
			m.applyFetch(result)
		case <-tick.C:
			m.updateSize()
			if time.Since(m.lastFetch) >= tuiFetchInterval {
				m.startFetch()
			}
			m.render(stdout)
		case ev, ok := <-keys:
			if !ok {
				return nil
			}
			m.handleKey(ev, stdout, stderr)
		}
	}
	return nil
}

// startFetch permits only one outstanding poll, preventing stale responses from
// overtaking newer ones. Capture every worker input before starting it.
func (m *tuiModel) startFetch() {
	if m.fetching {
		return
	}
	if m.results == nil {
		m.results = make(chan tuiFetchResult, 1)
	}
	m.fetching = true
	m.lastFetch = time.Now()
	client, addr, results := m.client, m.addr, m.results
	wallpaper, bad, muteRevision := m.wallpaper, m.wallpaperBad, m.muteRevision
	go func() {
		state, err := fetchState(client, addr)
		result := tuiFetchResult{state: state, err: err, wallpaper: wallpaper, wallpaperBad: bad, muteRevision: muteRevision}
		if err == nil {
			result.wallpaper, result.wallpaperBad = refreshWallpaper(client, addr, wallpaper, bad)
		}
		results <- result
	}()
}

func (m *tuiModel) applyFetch(result tuiFetchResult) {
	m.fetching = false
	if result.err != nil {
		m.fetchErr = "control API unreachable: " + result.err.Error()
		return
	}
	m.fetchErr = ""
	m.state = *result.state
	m.wallpaper, m.wallpaperBad = result.wallpaper, result.wallpaperBad
	if result.muteRevision == m.muteRevision {
		m.muted = result.state.Muted
	}
	// Keep a selected display stable when earlier displays disappear or reorder.
	if m.sessionID != "" {
		for i, s := range m.state.Sessions {
			if s.ID == m.sessionID {
				m.sessionSelected = i
				break
			}
		}
	}
	m.clampSelections()
	// Poll success must not erase command failures before the user can read them.
}

func fetchState(client *http.Client, addr string) (*tuiState, error) {
	resp, err := client.Get(controlURL(addr) + "/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, tuiHTTPError(resp.StatusCode, body)
	}
	var state tuiState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// filteredApps returns apps matching the search string.
func (m *tuiModel) filteredApps() []viewerApp {
	search := strings.ToLower(m.search)
	if search == "" {
		return m.state.Apps
	}
	out := make([]viewerApp, 0, len(m.state.Apps))
	for _, a := range m.state.Apps {
		if strings.Contains(strings.ToLower(a.Name), search) || strings.Contains(strings.ToLower(a.Package), search) {
			out = append(out, a)
		}
	}
	return out
}

// handleKey processes a key event.
func (m *tuiModel) handleKey(ev keyEvent, stdout io.Writer, stderr io.Writer) {
	if m.help {
		m.handleHelpKey(ev)
		return
	}

	switch ev.kind {
	case keyQuit:
		m.quitting = true
	case keyUp:
		m.moveSelection(-1)
	case keyDown:
		m.moveSelection(1)
	case keyHome:
		m.moveSelection(-len(m.state.Apps) - len(m.state.Sessions))
	case keyEnd:
		m.moveSelection(len(m.state.Apps) + len(m.state.Sessions))
	case keyPageUp:
		m.moveSelection(-m.selectionRows())
	case keyPageDown:
		m.moveSelection(m.selectionRows())
	case keyEnter:
		m.openSelected(stdout)
	case keyEsc:
		if m.searchFocus {
			m.searchFocus = false
			m.search = ""
			m.selected = 0
			m.scroll = 0
		}
	case keyTab:
		m.sessionsFocus = !m.sessionsFocus
		m.searchFocus = false
		m.clampSelections()
	case keyChar:
		m.handleChar(ev.rune, stdout)
	case keyBackspace:
		if m.searchFocus && len(m.search) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.search)
			m.search = m.search[:len(m.search)-size]
			m.filterAndClamp()
		}
	}
}

// handleChar dispatches a printable key. While the search box has focus every
// printable goes into the query; otherwise the documented command keys fire.
func (m *tuiModel) handleChar(r rune, stdout io.Writer) {
	if m.searchFocus {
		m.search += string(r)
		m.filterAndClamp()
		return
	}
	switch r {
	case 'q':
		m.quitting = true
	case 'j':
		m.moveSelection(1)
	case 'k':
		m.moveSelection(-1)
	case 'd':
		m.openDesktop(stdout)
	case '/':
		m.sessionsFocus = false
		m.searchFocus = true
	case 'o':
		m.cycleOrientation()
	case 'x':
		m.stopSelected(stdout)
	case 'm':
		m.toggleMute(stdout)
	case 'r':
		m.lastFetch = time.Time{} // force immediate refetch
	case '?':
		m.help = true
		m.helpScroll = 0
	}
}

var tuiHelpLines = [...]string{
	"help · ↑↓ scroll · Esc close",
	"Tab: apps/displays",
	"↑↓/jk: select",
	"Home/End: first/last",
	"PgUp/PgDn: page",
	"Enter: launch/focus",
	"/: search apps",
	"Esc: clear search",
	"o: orient display",
	"x: stop display",
	"d: mobile desktop",
	"m: toggle mute",
	"r: refresh",
	"q/Ctrl-C: quit",
}

func (m *tuiModel) handleHelpKey(ev keyEvent) {
	page := max(1, m.height-4)
	switch ev.kind {
	case keyQuit:
		m.quitting = true
	case keyEsc, keyEnter:
		m.help = false
	case keyUp:
		m.helpScroll--
	case keyDown:
		m.helpScroll++
	case keyPageUp:
		m.helpScroll -= page
	case keyPageDown:
		m.helpScroll += page
	case keyHome:
		m.helpScroll = 0
	case keyEnd:
		m.helpScroll = len(tuiHelpLines)
	case keyChar:
		switch ev.rune {
		case '?', 'q':
			m.help = false
		case 'j':
			m.helpScroll++
		case 'k':
			m.helpScroll--
		}
	}
	m.helpScroll = clampInt(m.helpScroll, 0, max(0, len(tuiHelpLines)-page))
}

func (m *tuiModel) filterAndClamp() {
	m.selected, m.scroll = 0, 0
	m.clampSelections()
}

// paneRows shares the exact row budgets used by rendering and navigation.
// A short narrow terminal shows only the focused pane; Tab still reaches both.
func (m *tuiModel) paneRows() (apps, info int) {
	body := max(0, m.height-4)
	if m.width >= tuiTwoColMinWidth {
		return body, body
	}
	if body < 10 {
		if m.sessionsFocus {
			return 0, body
		}
		return body, 0
	}
	return (body + 1) / 2, body / 2
}

func (m *tuiModel) selectionRows() int {
	apps, info := m.paneRows()
	if m.sessionsFocus {
		return max(1, info-1)
	}
	if apps >= 3 {
		return max(1, apps-2)
	}
	return 1
}

func tuiClampSelection(selected, scroll *int, count, visible int) {
	*selected = clampInt(*selected, 0, max(0, count-1))
	*scroll = clampInt(*scroll, 0, max(0, count-visible))
	if *selected < *scroll {
		*scroll = *selected
	}
	if *selected >= *scroll+visible {
		*scroll = *selected - visible + 1
	}
}

func (m *tuiModel) clampSelections() {
	appRows, infoRows := m.paneRows()
	if appRows >= 3 {
		appRows -= 2
	} else if appRows == 2 {
		appRows--
	}
	tuiClampSelection(&m.selected, &m.scroll, len(m.filteredApps()), max(1, appRows))
	tuiClampSelection(&m.sessionSelected, &m.sessionScroll, len(m.state.Sessions), max(1, infoRows-1))
	m.sessionID = ""
	if len(m.state.Sessions) > 0 {
		m.sessionID = m.state.Sessions[m.sessionSelected].ID
	}
}

func (m *tuiModel) moveSelection(delta int) {
	if m.sessionsFocus {
		m.sessionSelected += delta
	} else {
		m.selected += delta
	}
	m.clampSelections()
}

func (m *tuiModel) selectedSession() (viewerSession, bool) {
	if !m.sessionsFocus {
		m.statusMsg, m.statusErr = "Tab to open displays and select a session", true
		return viewerSession{}, false
	}
	m.clampSelections()
	if len(m.state.Sessions) == 0 {
		m.statusMsg, m.statusErr = "no open display selected", true
		return viewerSession{}, false
	}
	return m.state.Sessions[m.sessionSelected], true
}

func (m *tuiModel) openSelected(stdout io.Writer) {
	if m.sessionsFocus {
		s, ok := m.selectedSession()
		if !ok {
			return
		}
		if err := postJSON(m.client, m.addr+"/focus", map[string]any{"sessionId": s.ID}); err != nil {
			m.statusMsg, m.statusErr = "focus failed: "+err.Error(), true
			return
		}
		m.statusMsg, m.statusErr = "focused "+s.Name, false
		return
	}
	apps := m.filteredApps()
	if len(apps) == 0 || m.selected < 0 || m.selected >= len(apps) {
		return
	}
	app := apps[m.selected]
	body := map[string]any{"op": "launch", "mode": "app", "appId": app.ID, "width": 1280, "height": 720}
	if err := postJSON(m.client, m.addr+"/launch", body); err != nil {
		m.statusMsg = "launch failed: " + err.Error()
		m.statusErr = true
		return
	}
	m.statusMsg = "launched " + app.Name
	m.statusErr = false
	m.lastFetch = time.Time{}
}

func (m *tuiModel) openDesktop(stdout io.Writer) {
	body := map[string]any{"op": "launch", "mode": "desktop", "width": 1280, "height": 720}
	if err := postJSON(m.client, m.addr+"/launch", body); err != nil {
		m.statusMsg = "desktop failed: " + err.Error()
		m.statusErr = true
		return
	}
	m.statusMsg = "opened mobile desktop"
	m.statusErr = false
	m.lastFetch = time.Time{}
}

func (m *tuiModel) stopSelected(stdout io.Writer) {
	s, ok := m.selectedSession()
	if !ok {
		return
	}
	if err := postJSON(m.client, m.addr+"/stop", map[string]any{"sessionId": s.ID}); err != nil {
		m.statusMsg, m.statusErr = "stop failed: "+err.Error(), true
		return
	}
	m.statusMsg, m.statusErr = "stopped "+s.Name, false
	m.lastFetch = time.Time{}
}

func (m *tuiModel) toggleMute(stdout io.Writer) {
	next := !m.muted
	if err := postJSON(m.client, m.addr+"/mute", map[string]any{"mute": next}); err != nil {
		m.statusMsg, m.statusErr = "mute failed: "+err.Error(), true
		return
	}
	m.muted = next
	m.muteRevision++
	m.statusMsg, m.statusErr = fmt.Sprintf("audio mute %t", next), false
	m.lastFetch = time.Time{}
}

func (m *tuiModel) cycleOrientation() {
	s, ok := m.selectedSession()
	if !ok {
		return
	}
	if s.OrientationPending != "" {
		m.statusMsg, m.statusErr = "orientation "+s.OrientationPending+" pending · "+s.Name, false
		return
	}
	next := "landscape"
	switch s.Orientation {
	case "landscape":
		next = "portrait"
	case "portrait":
		next = "auto"
	}
	body := map[string]any{"sessionId": s.ID, "orientation": next}
	if err := postJSON(m.client, m.addr+"/orientation", body); err != nil {
		m.statusMsg, m.statusErr = "orientation failed: "+err.Error(), true
		return
	}
	m.statusMsg, m.statusErr = "orientation "+next+" requested · "+s.Name, false
	m.lastFetch = time.Time{}
}

// ---------- rendering ----------

// tuiColumns returns the left and right column widths for a terminal width.
// At tuiTwoColMinWidth columns and wider the app list gets ~55% and the
// detail/phone/wallpaper panes ~45%, separated by tuiGutter blank cells;
// narrower terminals collapse to a single column (right = 0).
func tuiColumns(total int) (left, right int) {
	if total < tuiTwoColMinWidth {
		return total, 0
	}
	right = total * 45 / 100
	left = total - right - tuiGutter
	if left < 20 {
		left = 20
		right = total - left - tuiGutter
	}
	return left, right
}

// updateSize refreshes the terminal width/height so the render can lay out the
// columns. It only runs on the event loop (never from a background
// goroutine), which keeps m.width/m.height race-free.
func (m *tuiModel) updateSize() {
	w, h, err := term.GetSize(m.fd)
	if err != nil || w <= 0 || h <= 0 {
		return
	}
	m.width = w
	m.height = h
}

// render keeps every frame within the current terminal size. Narrow terminals
// stack the panes, or show only the focused pane when there is too little height.
func (m *tuiModel) render(stdout io.Writer) {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.clampSelections()
	state, apps := m.state, m.filteredApps()
	header := tuiBold + " rfproto " + tuiReset + tuiMuted + "· phone workspace "
	if state.Connected {
		header += tuiAccent + "● live"
	} else {
		header += tuiDim + "○ disconnected"
	}
	status := m.statusMsg
	if status == "" {
		status = "waiting for phone"
		if state.Connected {
			status = "connected · " + state.Phone
		}
	}
	if m.fetchErr != "" {
		status = "[error] " + m.fetchErr
	} else if m.statusErr {
		status = "[error] " + status
	}
	lines := []string{tuiRow(tuiGround+tuiText, header, m.width)}
	if m.height > 1 {
		lines = append(lines, tuiRow(tuiGround+tuiMuted, tuiDisplayText(status), m.width))
	}
	bodyRows := max(0, m.height-4)
	if m.help {
		m.helpScroll = clampInt(m.helpScroll, 0, max(0, len(tuiHelpLines)-max(1, bodyRows)))
		for i := range bodyRows {
			text := ""
			if row := m.helpScroll + i; row < len(tuiHelpLines) {
				text = tuiHelpLines[row]
			}
			lines = append(lines, tuiRow(tuiGround+tuiText, text, m.width))
		}
	} else {
		appRows, infoRows := m.paneRows()
		leftW, rightW := tuiColumns(m.width)
		infoW := rightW
		if rightW == 0 {
			infoW = m.width
		}
		left := m.renderAppColumn(apps, state, leftW, appRows)
		right := m.renderInfoColumn(apps, state, m.wallpaper, infoW, infoRows)
		row := func(rows []string, i, width int) string {
			if i < len(rows) {
				return rows[i]
			}
			return tuiRow(tuiGround, "", width)
		}
		for i := range bodyRows {
			if rightW > 0 {
				lines = append(lines, row(left, i, leftW)+tuiGround+strings.Repeat(" ", tuiGutter)+row(right, i, rightW))
			} else if i < appRows {
				lines = append(lines, row(left, i, leftW))
			} else {
				lines = append(lines, row(right, i-appRows, infoW))
			}
		}
	}
	if m.height >= 3 {
		context := "apps · Enter launch · / search"
		if m.sessionsFocus {
			context = "displays · Enter focus · o orient · x stop"
		}
		lines = append(lines, tuiRow(tuiGround+tuiMuted, context, m.width))
	}
	if m.height >= 4 {
		hint := "Tab panes · / search · ? help · q quit"
		if m.width < 38 {
			hint = "Tab · ? help · q quit"
		}
		lines = append(lines, tuiRow(tuiGround+tuiDim, hint, m.width))
	}
	// CR before LF cancels deferred wrapping after a full-width row. No final
	// newline: drawing the terminal's last row must not scroll the screen.
	fmt.Fprint(stdout, tuiHome+tuiGround+strings.Join(lines, "\r\n")+tuiReset)
}

// renderAppColumn keeps app selection and search within its allotted rows.
func (m *tuiModel) renderAppColumn(apps []viewerApp, state tuiState, width, avail int) []string {
	if avail <= 0 {
		return nil
	}
	if m.searchFocus && avail < 3 {
		return []string{tuiRow(tuiGround+tuiAccent, "> "+m.search+"_", width)}
	}
	lines := make([]string, 0, avail)
	if avail >= 2 {
		head := "apps (" + strconvItoa(len(apps)) + "/" + strconvItoa(len(state.Apps)) + ")"
		if !m.sessionsFocus {
			head = "› " + head
		}
		lines = append(lines, tuiRow(tuiGround+tuiBold+tuiText, head, width))
	}
	visible := avail - len(lines)
	if avail >= 3 {
		visible--
	}
	for i := m.scroll; i < m.scroll+visible && i < len(apps); i++ {
		app := apps[i]
		prefix := tuiGround + tuiText
		marker := "  "
		if i == m.selected {
			prefix = tuiSelected + tuiSelectedText
			if !m.sessionsFocus {
				marker = "› "
			}
		}
		text := marker + tuiDisplayText(app.Name)
		if app.Package != "" {
			text += tuiMuted + "  " + tuiDisplayText(app.Package)
		}
		lines = append(lines, tuiRow(prefix, text, width))
	}
	for len(lines) < avail {
		if avail >= 3 && len(lines) == avail-1 {
			text := "/ search"
			if m.searchFocus {
				text = "> " + m.search + "_"
			} else if m.search != "" {
				text = "/ " + m.search
			}
			lines = append(lines, tuiRow(tuiGround+tuiAccent, text, width))
		} else {
			text := ""
			if len(apps) == 0 && len(lines) == 1 {
				text = "no matching apps"
			}
			lines = append(lines, tuiRow(tuiGround+tuiDim, text, width))
		}
	}
	return lines
}

// renderInfoColumn prioritizes selectable displays, then details and wallpaper.
func (m *tuiModel) renderInfoColumn(apps []viewerApp, state tuiState, wallpaper *wallpaperThumb, width, maxRows int) []string {
	if maxRows <= 0 {
		return nil
	}
	lines := make([]string, 0, maxRows)
	add := func(prefix, text string) {
		if len(lines) < maxRows {
			lines = append(lines, tuiRow(tuiGround+prefix, text, width))
		}
	}
	if maxRows >= 2 {
		title := "open displays (" + strconvItoa(len(state.Sessions)) + ")"
		if m.sessionsFocus {
			title = "› " + title
		}
		add(tuiBold+tuiText, title)
	}
	visible := maxRows - len(lines)
	for i := m.sessionScroll; i < len(state.Sessions) && i < m.sessionScroll+visible; i++ {
		s := state.Sessions[i]
		prefix, marker := tuiText, "  "
		if i == m.sessionSelected {
			prefix = tuiSelected + tuiSelectedText
			if m.sessionsFocus {
				marker = "› "
			}
		}
		detail := s.State + " · " + s.Orientation
		if s.OrientationPending != "" {
			detail += " → " + s.OrientationPending + " pending"
		}
		if s.OrientationError != "" {
			detail += " · " + s.OrientationError
		}
		add(prefix, marker+tuiDisplayText(s.Name)+" ["+tuiDisplayText(s.ID)+"] · "+tuiDisplayText(detail))
	}
	if len(state.Sessions) == 0 {
		add(tuiDim, "no open displays")
	}
	add(tuiDim, "")
	if len(apps) > 0 && m.selected < len(apps) {
		app := apps[m.selected]
		add(tuiBold+tuiText, "detail · "+tuiDisplayText(app.Name))
		add(tuiMuted, tuiDisplayText(app.Package)+" · user "+strconvItoa(app.UserID))
	}
	if state.Connected {
		add(tuiAccent, "● "+tuiDisplayText(state.Phone))
	} else {
		add(tuiDim, "○ no phone paired")
	}
	if state.AppLimit > 0 {
		add(tuiMuted, "app limit: "+strconvItoa(state.AppLimit))
	}
	if room := maxRows - len(lines) - 1; room >= 2 {
		add(tuiBold+tuiText, "wallpaper")
		rows := renderWallpaper(wallpaper, width)
		for _, line := range rows[:min(len(rows), room)] {
			lines = append(lines, line)
		}
	}
	return lines
}

// renderWallpaper renders the cached thumbnail as half-block rows: each cell
// draws "▀" (U+2580) with the top pixel as the 24-bit foreground color and the
// bottom pixel as the background color. A nil or empty cache renders the dim
// placeholder instead.
func renderWallpaper(thumb *wallpaperThumb, width int) []string {
	if thumb == nil || len(thumb.rows) == 0 {
		return []string{tuiRow(tuiGround+tuiDim, "  no wallpaper", width)}
	}
	lines := make([]string, 0, len(thumb.rows)+1)
	for _, row := range thumb.rows {
		var text strings.Builder
		text.WriteString("  ")
		for _, cell := range row {
			fmt.Fprintf(&text, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀",
				cell.top[0], cell.top[1], cell.top[2],
				cell.bottom[0], cell.bottom[1], cell.bottom[2])
		}
		text.WriteString(tuiGround) // black ground for the padded remainder
		lines = append(lines, tuiRow(tuiGround, text.String(), width))
	}
	if thumb.imgW > 0 && thumb.imgH > 0 {
		lines = append(lines, tuiRow(tuiGround+tuiDim, "  "+strconvItoa(thumb.imgW)+"x"+strconvItoa(thumb.imgH), width))
	}
	return lines
}

// tuiRow renders one fixed-width column row: prefix carries the row colors
// (its background fills the padding), text is clipped to the column width and
// padded with spaces, and attributes reset at the end of the row.
func tuiRow(prefix, text string, width int) string {
	if width <= 0 {
		return ""
	}
	text = tuiTruncate(text, width)
	if pad := width - tuiVisWidth(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return prefix + text + tuiReset
}

// tuiVisWidth counts the terminal cells s occupies, ignoring the SGR escape
// sequences it embeds.
func tuiVisWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == 0x1b:
			esc = true
		case esc:
			if r == 'm' {
				esc = false
			}
		default:
			n += tuiRuneWidth(r)
		}
	}
	return n
}

// tuiTruncate clips s to n visible cells, keeping escape sequences intact.
func tuiTruncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	count, esc := 0, false
	for _, r := range s {
		switch {
		case r == 0x1b:
			esc = true
		case esc:
			if r == 'm' {
				esc = false
			}
		default:
			width := tuiRuneWidth(r)
			if count+width > n {
				return b.String()
			}
			count += width
		}
		b.WriteRune(r)
	}
	return b.String()
}

func tuiRuneWidth(r rune) int {
	if unicode.IsControl(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f ||
		r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff ||
		r >= 0xfe10 && r <= 0xfe19 || r >= 0xfe30 && r <= 0xfe6f ||
		r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 ||
		r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

// API-provided names and errors are text, never terminal escape sequences.
func tuiDisplayText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// ---------- wallpaper thumbnail ----------

// wallpaperInfo mirrors the GET /wallpaper snapshot from the control API.
type wallpaperInfo struct {
	Path    string `json:"wallpaperPath"`
	Version uint64 `json:"wallpaperVersion"`
	Bytes   int    `json:"wallpaperBytes"`
	Width   int    `json:"wallpaperWidth"`
	Height  int    `json:"wallpaperHeight"`
}

// fetchWallpaper queries the control API for the wallpaper snapshot.
func fetchWallpaper(client *http.Client, addr string) (*wallpaperInfo, error) {
	resp, err := client.Get(controlURL(addr) + "/wallpaper")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var info wallpaperInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// refreshWallpaper runs in the poll worker and reuses unchanged pixel snapshots.
func refreshWallpaper(client *http.Client, addr string, cached *wallpaperThumb, bad uint64) (*wallpaperThumb, uint64) {
	info, err := fetchWallpaper(client, addr)
	if err != nil {
		return cached, bad
	}
	if info.Version == 0 || info.Path == "" || info.Bytes < tuiWallpaperMinB {
		return nil, 0
	}
	if cached != nil && cached.version == info.Version || bad == info.Version {
		return cached, bad
	}
	thumb, err := decodeWallpaperThumb(info)
	if err != nil {
		return nil, info.Version
	}
	return thumb, 0
}

// decodeWallpaperThumb opens the PNG at info.Path and scales it down to the
// thumbnail the render draws.
func decodeWallpaperThumb(info *wallpaperInfo) (*wallpaperThumb, error) {
	if info.Path == "" || info.Bytes < tuiWallpaperMinB {
		return nil, fmt.Errorf("no wallpaper")
	}
	f, err := os.Open(info.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	imgW, imgH := b.Dx(), b.Dy()
	if imgW <= 0 || imgH <= 0 {
		return nil, fmt.Errorf("empty wallpaper image")
	}
	cells, rows := thumbnailGeometry(imgW, imgH)
	return &wallpaperThumb{
		version: info.Version,
		imgW:    imgW,
		imgH:    imgH,
		rows:    scaleWallpaper(img, cells, rows),
	}, nil
}

// thumbnailGeometry picks the thumbnail size: nominally tuiThumbCells wide,
// keeping the image aspect (each terminal cell pairs two pixel rows). Very
// tall images shrink in width instead, so the height stays under
// tuiThumbMaxRows.
func thumbnailGeometry(imgW, imgH int) (cells, rows int) {
	cells = tuiThumbCells
	rows = int(math.Round(float64(cells) * float64(imgH) / (2 * float64(imgW))))
	if rows < 1 {
		rows = 1
	}
	if rows > tuiThumbMaxRows {
		rows = tuiThumbMaxRows
		cells = int(math.Round(float64(rows) * 2 * float64(imgW) / float64(imgH)))
		if cells < 4 {
			cells = 4
		}
	}
	return cells, rows
}

// scaleWallpaper nearest-samples img into half-block rows. Each output cell
// carries a top and bottom pixel: the render draws "▀" with the top pixel as
// the foreground color and the bottom pixel as the background color. Sample
// coordinates are clamped to the image bounds.
func scaleWallpaper(img image.Image, cells, rows int) [][]tuiPixel {
	b := img.Bounds()
	imgW, imgH := b.Dx(), b.Dy()
	if cells <= 0 || rows <= 0 || imgW <= 0 || imgH <= 0 {
		return nil
	}
	pixelRows := rows * 2 // two sampled pixels per cell row
	out := make([][]tuiPixel, rows)
	for r := range rows {
		row := make([]tuiPixel, cells)
		for c := range cells {
			fx := (float64(c) + 0.5) * float64(imgW) / float64(cells)
			row[c].top = samplePixel(img, b, fx, float64(2*r)*float64(imgH)/float64(pixelRows))
			row[c].bottom = samplePixel(img, b, fx, float64(2*r+1)*float64(imgH)/float64(pixelRows))
		}
		out[r] = row
	}
	return out
}

// samplePixel nearest-samples one pixel of img, clamped to its bounds.
func samplePixel(img image.Image, b image.Rectangle, fx, fy float64) rgb {
	x := clampInt(b.Min.X+int(math.Floor(fx)), b.Min.X, b.Max.X-1)
	y := clampInt(b.Min.Y+int(math.Floor(fy)), b.Min.Y, b.Max.Y-1)
	r, g, bl, _ := img.At(x, y).RGBA()
	return rgb{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func strconvItoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// ---------- key reading ----------

type keyKind int

const (
	keyNone keyKind = iota
	keyChar
	keyEnter
	keyBackspace
	keyEsc
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyTab
	keyQuit
)

type keyEvent struct {
	kind keyKind
	rune rune
}

// keyDecoder retains incomplete UTF-8 and escape sequences across reads.
type keyDecoder struct {
	pending []byte
}

func readKeys(fd int, done <-chan struct{}) <-chan keyEvent {
	ch := make(chan keyEvent, 16)
	go func() {
		defer close(ch)
		var decoder keyDecoder
		var buf [256]byte
		emit := func(ev keyEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-done:
				return false
			}
		}
		for {
			select {
			case <-done:
				return
			default:
			}
			fds := [1]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds[:], 40)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				if !decoder.expireEscape(emit) {
					return
				}
				continue
			}
			n, err = syscall.Read(fd, buf[:])
			if err == syscall.EINTR {
				continue
			}
			if err != nil || n == 0 || !decoder.feed(buf[:n], emit) {
				return
			}
		}
	}()
	return ch
}

// Only a lone Escape needs a timeout. An incomplete CSI or UTF-8 rune can wait
// for the next read without producing spurious search characters.
func (d *keyDecoder) expireEscape(emit func(keyEvent) bool) bool {
	if len(d.pending) == 1 && d.pending[0] == 27 {
		d.pending = d.pending[:0]
		return emit(keyEvent{kind: keyEsc})
	}
	return true
}

func (d *keyDecoder) feed(data []byte, emit func(keyEvent) bool) bool {
	buffer := append(d.pending, data...)
	d.pending = buffer
	defer func() {
		n := copy(buffer, d.pending)
		d.pending = buffer[:n]
	}()
	for len(d.pending) > 0 {
		c := d.pending[0]
		ev, consumed := keyEvent{}, 1
		if c == 27 {
			if len(d.pending) == 1 {
				return true
			}
			if d.pending[1] == '[' || d.pending[1] == 'O' {
				end := 2
				for end < len(d.pending) && !(d.pending[end] >= 0x40 && d.pending[end] <= 0x7e) {
					end++
				}
				if end == len(d.pending) {
					if len(d.pending) >= 32 {
						d.pending = d.pending[:0]
					}
					return true
				}
				ev.kind = decodeCSI(d.pending[2 : end+1])
				consumed = end + 1
			} else {
				ev.kind = keyEsc
			}
		} else {
			switch c {
			case 3:
				ev.kind = keyQuit
			case 8, 127:
				ev.kind = keyBackspace
			case 13, 10:
				ev.kind = keyEnter
			case 9:
				ev.kind = keyTab
			default:
				if !utf8.FullRune(d.pending) {
					return true
				}
				r, size := utf8.DecodeRune(d.pending)
				consumed = size
				if unicode.IsPrint(r) && !(r == utf8.RuneError && size == 1) {
					ev = keyEvent{kind: keyChar, rune: r}
				}
			}
		}
		d.pending = d.pending[consumed:]
		if ev.kind != keyNone && !emit(ev) {
			return false
		}
	}
	return true
}

func decodeCSI(seq []byte) keyKind {
	if len(seq) == 0 {
		return keyNone
	}
	switch seq[len(seq)-1] {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case 'Z':
		return keyTab
	}
	switch string(seq) {
	case "1~", "7~":
		return keyHome
	case "4~", "8~":
		return keyEnd
	case "5~":
		return keyPageUp
	case "6~":
		return keyPageDown
	}
	return keyNone
}

// ---------- HTTP helpers ----------

func postJSON(client *http.Client, url string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.Post(controlURL(url), "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tuiHTTPError(resp.StatusCode, response)
	}
	return nil
}

func tuiHTTPError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &response) == nil && response.Error != "" {
		message = response.Error
	}
	if message == "" {
		return fmt.Errorf("HTTP %d", status)
	}
	return fmt.Errorf("HTTP %d: %s", status, tuiDisplayText(message))
}
