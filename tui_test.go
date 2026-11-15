package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func TestTUIKeyDecoderSplitReads(t *testing.T) {
	input := []byte("\x1b[A\x1b[6~\x1bOHé界🙂\t\x7f")
	want := []keyEvent{
		{kind: keyUp}, {kind: keyPageDown}, {kind: keyHome},
		{kind: keyChar, rune: 'é'}, {kind: keyChar, rune: '界'},
		{kind: keyChar, rune: '🙂'}, {kind: keyTab}, {kind: keyBackspace},
	}
	for split := range len(input) + 1 {
		var decoder keyDecoder
		var got []keyEvent
		emit := func(ev keyEvent) bool { got = append(got, ev); return true }
		decoder.feed(input[:split], emit)
		decoder.feed(input[split:], emit)
		if !reflect.DeepEqual(got, want) || len(decoder.pending) != 0 {
			t.Fatalf("split %d: got %#v, pending %q", split, got, decoder.pending)
		}
	}
	var decoder keyDecoder
	var got []keyEvent
	emit := func(ev keyEvent) bool { got = append(got, ev); return true }
	for _, b := range input {
		decoder.feed([]byte{b}, emit)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bytewise input: %#v", got)
	}
	decoder.feed([]byte{27}, emit)
	decoder.expireEscape(emit)
	if got[len(got)-1].kind != keyEsc || len(decoder.pending) != 0 {
		t.Fatal("a lone Escape did not expire")
	}
	decoder.feed([]byte("\x1b["), emit)
	decoder.expireEscape(emit)
	decoder.feed([]byte("1;5A"), emit)
	if got[len(got)-1].kind != keyUp {
		t.Fatal("incomplete CSI was lost at the Escape timeout")
	}
}

func TestTUISearchBackspaceUTF8(t *testing.T) {
	m := tuiModel{width: 80, height: 24, sessionsFocus: true}
	m.handleChar('/', io.Discard)
	for _, r := range "é界🙂q" {
		m.handleChar(r, io.Discard)
	}
	if m.sessionsFocus || m.quitting || m.search != "é界🙂q" {
		t.Fatalf("search focus: %#v", m)
	}
	for _, want := range []string{"é界🙂", "é界", "é", ""} {
		m.handleKey(keyEvent{kind: keyBackspace}, io.Discard, io.Discard)
		if m.search != want || !utf8.ValidString(m.search) {
			t.Fatalf("backspace: got %q, want %q", m.search, want)
		}
	}
}

func tuiTestModel(width, height int) tuiModel {
	m := tuiModel{width: width, height: height}
	for i := range 30 {
		m.state.Apps = append(m.state.Apps, viewerApp{ID: fmt.Sprintf("a%d", i), Name: fmt.Sprintf("A%02d", i)})
		m.state.Sessions = append(m.state.Sessions, viewerSession{ID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("S%02d", i)})
	}
	return m
}

func TestTUISelectionNavigation(t *testing.T) {
	for _, size := range [][2]int{{120, 24}, {80, 24}, {40, 10}, {20, 7}, {20, 6}, {20, 5}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := tuiTestModel(size[0], size[1])
			press := func(kind keyKind) { m.handleKey(keyEvent{kind: kind}, io.Discard, io.Discard) }
			press(keyEnd)
			if m.selected != 29 || m.selected < m.scroll || m.selected >= m.scroll+m.selectionRows() {
				t.Fatalf("End hid selection: selected=%d scroll=%d", m.selected, m.scroll)
			}
			var frame bytes.Buffer
			m.render(&frame)
			if !strings.Contains(frame.String(), "A29") {
				t.Fatal("last app is not visible after End")
			}
			press(keyHome)
			if m.selected != 0 || m.scroll != 0 {
				t.Fatal("Home did not reset selection and scroll")
			}
			page := m.selectionRows()
			press(keyPageDown)
			if m.selected != page {
				t.Fatalf("PgDown selected %d, want %d", m.selected, page)
			}
			press(keyPageUp)
			if m.selected != 0 {
				t.Fatal("PgUp did not move selection back")
			}
			press(keyTab)
			press(keyEnd)
			if !m.sessionsFocus || m.sessionSelected != 29 || m.selected != 0 || m.sessionID != "s29" {
				t.Fatal("Tab did not give displays independent selection")
			}
			frame.Reset()
			m.render(&frame)
			if !strings.Contains(frame.String(), "S29") {
				t.Fatal("selected display is not visible")
			}
			press(keyHome)
			if m.sessionSelected != 0 || m.sessionScroll != 0 {
				t.Fatal("Home did not reset display selection and scroll")
			}
			page = m.selectionRows()
			press(keyPageDown)
			if m.sessionSelected != page {
				t.Fatal("PgDown did not move display selection by a page")
			}
			press(keyTab)
			if m.sessionsFocus || m.selected != 0 {
				t.Fatal("returning to apps lost app selection")
			}
		})
	}
}

func TestTUICommandsUseSelectedSession(t *testing.T) {
	type request struct{ path, id, orientation string }
	requestCh := make(chan request, 16)
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID   string `json:"sessionId"`
			Orientation string `json:"orientation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requestCh <- request{r.URL.Path, body.SessionID, body.Orientation}
		if fail.Load() {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"display is closing"}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	m := tuiModel{addr: strings.TrimPrefix(server.URL, "http://"), client: server.Client(), width: 100, height: 24}
	m.state.Apps = []viewerApp{{ID: "app", Name: "Twin"}}
	m.state.Sessions = []viewerSession{{ID: "s1", Name: "Twin"}, {ID: "s2", Name: "Twin"}}
	m.stopSelected(io.Discard)
	if !m.statusErr || len(requestCh) != 0 {
		t.Fatal("an app label was incorrectly treated as a session target")
	}
	m.handleKey(keyEvent{kind: keyTab}, io.Discard, io.Discard)
	m.moveSelection(1)
	m.cycleOrientation()
	if m.state.Sessions[1].Orientation != "" || !strings.Contains(m.statusMsg, "requested") {
		t.Fatal("request acceptance was mistaken for phone confirmation")
	}
	m.openSelected(io.Discard)
	m.stopSelected(io.Discard)
	m.applyFetch(tuiFetchResult{state: &tuiState{Sessions: []viewerSession{{ID: "s1", Name: "Twin", Orientation: "auto"}, {ID: "s2", Name: "Twin", Orientation: "landscape"}}}})
	m.moveSelection(-1)
	m.cycleOrientation()
	m.moveSelection(1)
	m.cycleOrientation()
	m.applyFetch(tuiFetchResult{state: &tuiState{Sessions: []viewerSession{{ID: "s1", Name: "Twin", Orientation: "auto"}, {ID: "s2", Name: "Twin", Orientation: "portrait"}}}})
	fail.Store(true)
	m.cycleOrientation()
	if m.state.Sessions[1].Orientation != "portrait" || !m.statusErr || !strings.Contains(m.statusMsg, "display is closing") {
		t.Fatal("failed orientation advanced its cycle or hid the API error")
	}
	fail.Store(false)
	m.cycleOrientation()
	want := []request{
		{"/orientation", "s2", "landscape"}, {"/focus", "s2", ""}, {"/stop", "s2", ""},
		{"/orientation", "s1", "landscape"}, {"/orientation", "s2", "portrait"},
		{"/orientation", "s2", "auto"}, {"/orientation", "s2", "auto"},
	}
	var requests []request
	for len(requestCh) > 0 {
		requests = append(requests, <-requestCh)
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests: %#v, want %#v", requests, want)
	}
}

func TestTUIMuteUsesAcknowledgedState(t *testing.T) {
	requestCh := make(chan bool, 8)
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mute bool `json:"mute"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requestCh <- body.Mute
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"media not running"}`)
		}
	}))
	defer server.Close()
	m := tuiModel{addr: strings.TrimPrefix(server.URL, "http://"), client: server.Client(), width: 80, height: 24}
	m.applyFetch(tuiFetchResult{state: &tuiState{Muted: true, Audio: viewerAudio{State: "connected"}}})
	m.toggleMute(io.Discard)
	if m.muted || m.statusErr {
		t.Fatal("snapshot mute state was ignored")
	}
	m.applyFetch(tuiFetchResult{state: &tuiState{Muted: true}, muteRevision: 0})
	if m.muted {
		t.Fatal("an older poll undid a successful mute command")
	}
	m.toggleMute(io.Discard)
	fail.Store(true)
	m.toggleMute(io.Discard)
	if !m.muted || !m.statusErr || !strings.Contains(m.statusMsg, "media not running") {
		t.Fatal("failed mute changed local state or concealed the error")
	}
	var requests []bool
	for len(requestCh) > 0 {
		requests = append(requests, <-requestCh)
	}
	if !reflect.DeepEqual(requests, []bool{false, true, false}) {
		t.Fatalf("mute requests: %v", requests)
	}
	m.applyFetch(tuiFetchResult{state: &tuiState{Muted: false}, muteRevision: m.muteRevision})
	if m.muted {
		t.Fatal("a current snapshot did not reflect an external mute change")
	}
}

func TestTUIPollPublishesSnapshots(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			close(started)
			<-release
			fmt.Fprint(w, `{"connected":true,"phone":"Phone","muted":true,"apps":[{"id":"new","name":"New"}]}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	m := tuiTestModel(80, 24)
	m.addr, m.client = strings.TrimPrefix(server.URL, "http://"), server.Client()
	m.startFetch()
	<-started
	m.startFetch() // single-flight: must not initiate a second request
	for range 20 {
		m.handleChar('j', io.Discard)
		m.render(io.Discard)
	}
	if m.state.Apps[0].ID != "a0" {
		t.Fatal("poll worker mutated the model")
	}
	m.statusMsg, m.statusErr = "command failed", true
	close(release)
	select {
	case result := <-m.results:
		if m.state.Apps[0].ID != "a0" {
			t.Fatal("unapplied poll changed model state")
		}
		m.applyFetch(result)
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not finish")
	}
	if !m.state.Connected || !m.muted || m.state.Apps[0].ID != "new" || m.fetching {
		t.Fatal("poll result was not applied")
	}
	if !m.statusErr || m.statusMsg != "command failed" {
		t.Fatal("poll erased a command error")
	}
	m.applyFetch(tuiFetchResult{err: errors.New("offline")})
	if !strings.Contains(m.fetchErr, "offline") {
		t.Fatal("poll failure was concealed")
	}
	m.applyFetch(tuiFetchResult{state: &tuiState{Connected: true}})
	if m.fetchErr != "" {
		t.Fatal("recovery retained a stale connection error")
	}
}

func TestTUIPollPreservesSessionIdentity(t *testing.T) {
	m := tuiTestModel(100, 24)
	m.sessionsFocus = true
	m.moveSelection(5)
	m.applyFetch(tuiFetchResult{state: &tuiState{Sessions: []viewerSession{{ID: "s5", Orientation: "portrait"}, {ID: "other"}}}})
	if m.sessionID != "s5" || m.sessionSelected != 0 || m.state.Sessions[0].Orientation != "portrait" {
		t.Fatal("reordering displays retargeted the selection")
	}
}

var tuiTestSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestTUIRenderingFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 24}, {60, 12}, {40, 10}, {20, 7}, {10, 5}, {1, 1}} {
		for _, focus := range []bool{false, true} {
			for _, help := range []bool{false, true} {
				m := tuiTestModel(size[0], size[1])
				m.sessionsFocus, m.help = focus, help
				m.state.Apps[0].Name = "界🙂e\u0301" + strings.Repeat("long", 50) + "\n\x1b[2J"
				m.statusMsg = strings.Repeat("status", 100) + "\n\x1b[2J"
				m.wallpaper = &wallpaperThumb{rows: [][]tuiPixel{make([]tuiPixel, 24), make([]tuiPixel, 24)}}
				var frame bytes.Buffer
				m.render(&frame)
				text := strings.TrimPrefix(frame.String(), tuiHome)
				text = tuiTestSGR.ReplaceAllString(text, "")
				lines := strings.Split(text, "\r\n")
				if len(lines) != size[1] {
					t.Fatalf("%v focus=%v help=%v: %d rows", size, focus, help, len(lines))
				}
				for _, line := range lines {
					cells := 0
					for _, r := range line {
						switch {
						case unicode.IsControl(r):
							t.Fatalf("unescaped control in rendered row: %q", line)
						case r == '界' || r == '🙂':
							cells += 2
						case unicode.Is(unicode.Mn, r):
						default:
							cells++
						}
					}
					if cells != size[0] {
						t.Fatalf("%v: row occupies %d cells: %q", size, cells, line)
					}
				}
			}
		}
	}
}

func TestTUIReadKeysStopsOnCancellationAndEOF(t *testing.T) {
	for _, eof := range []bool{false, true} {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		keys := readKeys(int(reader.Fd()), done)
		if eof {
			writer.Close()
		} else {
			close(done)
		}
		select {
		case _, ok := <-keys:
			if ok {
				t.Fatal("unexpected key from empty pipe")
			}
		case <-time.After(time.Second):
			t.Fatal("key reader did not stop")
		}
		reader.Close()
		if !eof {
			writer.Close()
		}
	}
}

func TestTUIWallpaperSamplesBothHalvesAndFullHeight(t *testing.T) {
	img := image.NewRGBA(image.Rect(10, 20, 11, 24))
	for y := 20; y < 24; y++ {
		img.SetRGBA(10, y, color.RGBA{R: uint8(y), A: 255})
	}
	rows := scaleWallpaper(img, 1, 2)
	if rows[0][0].top[0] != 20 || rows[0][0].bottom[0] != 21 || rows[1][0].top[0] != 22 || rows[1][0].bottom[0] != 23 {
		t.Fatalf("thumbnail discarded source rows: %#v", rows)
	}
}

func TestTUIHelpRemainsReachableInShortTerminal(t *testing.T) {
	m := tuiModel{width: 40, height: 7}
	m.handleChar('?', io.Discard)
	m.handleKey(keyEvent{kind: keyEnd}, io.Discard, io.Discard)
	var frame bytes.Buffer
	m.render(&frame)
	if !strings.Contains(frame.String(), "q/Ctrl-C: quit") {
		t.Fatal("short-terminal help could not reach its final controls")
	}
	m.handleKey(keyEvent{kind: keyHome}, io.Discard, io.Discard)
	frame.Reset()
	m.render(&frame)
	if !strings.Contains(frame.String(), "help ·") || m.helpScroll != 0 {
		t.Fatal("help Home did not return to its first row")
	}
	m.handleKey(keyEvent{kind: keyQuit}, io.Discard, io.Discard)
	if !m.quitting {
		t.Fatal("Ctrl-C was trapped by help")
	}
}

func TestTUISharedUnixControlTransport(t *testing.T) {
	_, addr := startTestControlSocket(t, t.TempDir())
	h := pairedHub(t)
	withStreams(t, h)
	withFrontend(t, nil)
	withFrontendAssets(t, nil)
	client, base, err := newControlClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	state, err := fetchState(client, base)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Connected || state.Phone != "Test phone" || state.Muted {
		t.Fatalf("UNIX state = %+v", state)
	}
	if _, err := fetchWallpaper(client, base); err != nil {
		t.Fatalf("UNIX wallpaper: %v", err)
	}
	if err := postJSON(client, base+"/stop", map[string]any{"sessionId": "missing"}); err != nil {
		t.Fatalf("UNIX POST: %v", err)
	}
}

func TestTUIOrientationWaitsForPhoneConfirmation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	m := tuiModel{addr: server.URL, client: server.Client(), sessionsFocus: true, width: 180, height: 24}
	m.applyFetch(tuiFetchResult{state: &tuiState{Sessions: []viewerSession{{ID: "display", Name: "Clock", State: "connected", Orientation: "landscape", OrientationPending: "portrait"}}}})
	m.cycleOrientation()
	if requests.Load() != 0 || !strings.Contains(m.statusMsg, "pending") {
		t.Fatal("a pending remote resize was submitted again")
	}
	m.applyFetch(tuiFetchResult{state: &tuiState{Sessions: []viewerSession{{ID: "display", Name: "Clock", State: "connected", Orientation: "landscape", OrientationError: "phone rejected resize"}}}})
	var rendered bytes.Buffer
	m.render(&rendered)
	if !strings.Contains(rendered.String(), "phone rejected resize") {
		t.Fatal("phone orientation failure was hidden")
	}
	m.cycleOrientation()
	if requests.Load() != 1 || m.state.Sessions[0].Orientation != "landscape" || !strings.Contains(m.statusMsg, "portrait requested") {
		t.Fatal("the next request was not based on confirmed phone orientation")
	}
}
