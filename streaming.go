package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	svcPhoneDesktop = 1
	svcAppStream    = 15
	svcPhoneAudio   = 21
)

// The SDL workspace terminates WebRTC with the phone. This hub owns signaling
// and remote resources; the authenticated control socket remains the authority
// for every launch, input event, and stop.
var streams *viewerHub

// frontendAssets is the read-only wallpaper/status observer installed by serve,
// so the control API can expose wallpaper and phone status without a WebSocket.
var frontendAssets *frontendObserver

type rtcDescription struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type rtcCandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
}

type viewerCommand struct {
	Op          string          `json:"op"`
	Mode        string          `json:"mode,omitempty"`
	AppID       string          `json:"appId,omitempty"`
	SessionID   string          `json:"sessionId,omitempty"`
	MediaID     string          `json:"mediaId,omitempty"`
	Orientation string          `json:"orientation,omitempty"`
	Width       int             `json:"width,omitempty"`
	Height      int             `json:"height,omitempty"`
	Description *rtcDescription `json:"description,omitempty"`
	Candidate   *rtcCandidate   `json:"candidate,omitempty"`
	Input       *viewerInput    `json:"input,omitempty"`
}

type viewerApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Package string `json:"package"`
	UserID  int    `json:"userId"`
}

type catalogApp struct {
	Pkg        string `json:"pkg"`
	Cmp        string `json:"cmp"`
	Name       string `json:"name"`
	UserID     int    `json:"userId"`
	ShortcutID string `json:"shortcutId,omitempty"`
}

func (a catalogApp) id() string {
	return a.Pkg + "/" + a.Cmp + ":" + strconv.Itoa(a.UserID) + ":" + a.ShortcutID
}

type viewerSession struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mode               string `json:"mode"`
	State              string `json:"state"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	DisplayID          int    `json:"displayId"`
	Rotation           int    `json:"rotation"`
	Touch              bool   `json:"touch"`
	MediaID            string `json:"mediaId"`
	Error              string `json:"error,omitempty"`
	Orientation        string `json:"orientation"`
	OrientationPending string `json:"orientationPending,omitempty"`
	OrientationError   string `json:"orientationError,omitempty"`
}

type viewerAudio struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

type viewerState struct {
	Connected bool            `json:"connected"`
	Phone     string          `json:"phone"`
	Apps      []viewerApp     `json:"apps"`
	AppLimit  int             `json:"appLimit"`
	Sessions  []viewerSession `json:"sessions"`
	Audio     viewerAudio     `json:"audio"`
}

type streamSession struct {
	viewerSession
	app                         catalogApp
	wireID                      int64
	deadline                    time.Time
	launched                    bool
	keys                        map[int]bool
	buttons                     map[int]viewerInput
	touches                     map[int]viewerInput
	launchWidth, launchHeight   int
	naturalWidth, naturalHeight int
	orientationDeadline         time.Time
}

type audioSession struct {
	viewerAudio
	wireID    int64
	requestID int32
	deadline  time.Time
}

type viewerHub struct {
	mediaInterface string // Optional ICE interface, set before serving.
	mu             sync.Mutex
	control        *liveSession
	apps           []viewerApp // Published snapshots are immutable.
	catalog        map[string]catalogApp
	appLimit       int
	sessions       map[string]*streamSession
	order          []*streamSession
	audio          *audioSession
	instance       int32
	inputSeq       uint64
	nextSession    uint64
	nextMedia      uint64
	nextAudio      int32
	events         chan any
	eventsOpen     bool
	done           chan struct{}
	closed         bool
}

func newViewerHub() *viewerHub {
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(fmt.Sprintf("streaming owner identity: %v", err))
	}
	instance := int32(binary.BigEndian.Uint32(seed[:]) & 0x7fffffff)
	if instance == 0 {
		instance = 1
	}
	h := &viewerHub{
		catalog: make(map[string]catalogApp), sessions: make(map[string]*streamSession),
		instance: instance, done: make(chan struct{}),
	}
	go h.maintain()
	return h
}

func (h *viewerHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
	if h.eventsOpen {
		close(h.events)
	}
	h.events, h.eventsOpen = nil, false
	h.stopAllLocked()
}

func (h *viewerHub) state() viewerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stateLocked()
}

func (h *viewerHub) stateLocked() viewerState {
	s := viewerState{Connected: h.control != nil, Apps: h.apps, AppLimit: h.appLimit,
		Sessions: make([]viewerSession, 0, len(h.order))}
	if h.control != nil {
		s.Phone = h.control.name
	}
	for _, session := range h.order {
		s.Sessions = append(s.Sessions, session.viewerSession)
	}
	if h.audio != nil {
		s.Audio = h.audio.viewerAudio
	}
	return s
}

func (h *viewerHub) emitLocked(event any) {
	if !h.eventsOpen {
		return
	}
	select {
	case h.events <- event:
	default:
		// A stalled viewer must not stall the phone's control receive loop.
		// Closing the subscription makes the native workspace release its sessions.
		close(h.events)
		h.eventsOpen = false
	}
}

func (h *viewerHub) publishLocked() {
	if h.eventsOpen {
		h.emitLocked(map[string]any{"type": "state", "state": h.stateLocked()})
	}
}

func (h *viewerHub) errorLocked(err error) {
	log.Printf("[stream] %v", err)
	h.emitLocked(map[string]any{"type": "error", "message": err.Error()})
}

func (h *viewerHub) subscribe() (<-chan any, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, errors.New("phone workspace is closed")
	}
	if h.events != nil {
		return nil, nil, errors.New("a phone workspace is already open")
	}
	events := make(chan any, 128)
	h.events, h.eventsOpen = events, true
	h.publishLocked()
	if h.control != nil {
		if err := h.sendCommandLocked(svcAppList, 2, nil); err != nil {
			h.errorLocked(err)
		}
	}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.events != events {
			return
		}
		if h.eventsOpen {
			close(events)
		}
		h.events, h.eventsOpen = nil, false
		h.stopAllLocked()
	}
	return events, cancel, nil
}

func (h *viewerHub) controlConnected(s *liveSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.control != nil && h.control != s {
		h.stopAllLocked()
	}
	h.control = s
	h.apps, h.catalog, h.appLimit = nil, make(map[string]catalogApp), 0
	h.publishLocked()
	if h.eventsOpen {
		if err := h.sendCommandLocked(svcAppList, 2, nil); err != nil {
			h.errorLocked(err)
		}
	}
}

func (h *viewerHub) controlDisconnected(s *liveSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.control != s {
		return
	}
	h.control = nil
	h.sessions, h.order, h.audio = make(map[string]*streamSession), nil, nil
	h.apps, h.catalog, h.appLimit = nil, make(map[string]catalogApp), 0
	h.publishLocked()
}

func (h *viewerHub) sendLocked(service int, payload any) error {
	if h.control == nil {
		return errors.New("no authenticated phone is connected")
	}
	out, err := json.Marshal(svcMsg{DeviceID: ourInfo.DeviceID, SrcServiceID: service,
		ServiceID: service, MsgID: nextWireMsgID(), SenderID: senderIDMobileDesktop, Data: payload})
	if err != nil {
		return err
	}
	return h.control.send("stream", out)
}

func (h *viewerHub) sendCommand(service, cmd int, data any) error {
	h.mu.Lock()
	control := h.control
	h.mu.Unlock()
	if control == nil {
		return errors.New("no authenticated phone is connected")
	}
	out, err := json.Marshal(svcMsg{DeviceID: ourInfo.DeviceID, SrcServiceID: service,
		ServiceID: service, MsgID: nextWireMsgID(), SenderID: senderIDMobileDesktop,
		Data: streamCommandData{Ver: "1", Cmd: cmd, Data: data}})
	if err != nil {
		return err
	}
	return control.send("stream", out)
}

type streamCommandData struct {
	Ver  string `json:"ver"`
	Cmd  int    `json:"cmd"`
	Data any    `json:"data,omitempty"`
}

func (h *viewerHub) sendCommandLocked(service, cmd int, data any) error {
	return h.sendLocked(service, streamCommandData{Ver: "1", Cmd: cmd, Data: data})
}

type desktopRequest struct {
	Ver        string            `json:"ver"`
	Cmd        int               `json:"cmd"`
	View       int               `json:"view,omitempty"`
	Resolution *streamResolution `json:"resolution,omitempty"`
	DrawerMode bool              `json:"drawerMode"`
	Reply      int               `json:"reply,omitempty"`
}

type streamResolution struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type streamLaunch struct {
	Pkg                  string  `json:"pkg"`
	Cmp                  string  `json:"cmp,omitempty"`
	UserID               int     `json:"userId"`
	ShortcutID           string  `json:"shortcutId,omitempty"`
	InstanceID           int32   `json:"instanceId"`
	Source               int     `json:"source"`
	Port                 int     `json:"port"`
	Width                int     `json:"width"`
	Height               int     `json:"height"`
	PhyWidth             int     `json:"phyWidth"`
	PhyHeight            int     `json:"phyHeight"`
	Align                int     `json:"align"`
	DisplayWidth         int     `json:"displayWidth"`
	DisplayHeight        int     `json:"displayHeight"`
	DisplayScale         float64 `json:"displayScale"`
	DisplayDensity       int     `json:"displayDensity"`
	SupportedMediaFormat int     `json:"supportedMediaFormat"`
	SupportWebRTC        int     `json:"supportWebRTC"`
	SupportCustomDisplay bool    `json:"supportCustomDisplay"`
	VRSupportUnfolded    bool    `json:"vrSupportUnfolded"`
}

func (h *viewerHub) launchVideoLocked(s *streamSession) error {
	if s.launched {
		return nil
	}
	err := h.sendCommandLocked(svcAppStream, 1, streamLaunch{
		Pkg: s.app.Pkg, Cmp: s.app.Cmp, UserID: s.app.UserID, ShortcutID: s.app.ShortcutID,
		InstanceID: h.instance, Source: 1, Width: s.Width, Height: s.Height,
		DisplayWidth: s.Width, DisplayHeight: s.Height, DisplayScale: 1, DisplayDensity: 160,
		SupportedMediaFormat: 1, SupportWebRTC: 1,
	})
	if err == nil {
		s.launched = true
	}
	return err
}

type streamResize struct {
	streamLaunch
	DisplayID int `json:"displayId"`
}

// requestOrientationLocked changes the phone's virtual display, not the local
// picture. The producer acknowledges service-15 cmd 17 with cmd 26.
func (h *viewerHub) requestOrientationLocked(cmd viewerCommand) (any, error) {
	s := h.sessions[cmd.SessionID]
	if s == nil || s.DisplayID < 0 || s.State != "connected" {
		return nil, errors.New("phone display is not connected")
	}
	mode := strings.ToLower(cmd.Orientation)
	if mode != "auto" && mode != "landscape" && mode != "portrait" {
		return nil, errors.New("orientation must be auto, landscape, or portrait")
	}
	if s.OrientationPending != "" {
		return nil, errors.New("the phone is still processing the previous orientation request")
	}
	w, height := s.naturalWidth, s.naturalHeight
	if w <= 0 || height <= 0 {
		w, height = s.Width, s.Height
	}
	switch mode {
	case "landscape":
		w, height = max(w, height), min(w, height)
	case "portrait":
		w, height = min(w, height), max(w, height)
	}
	// Display dimensions on the wire use the unrotated raster, as does input.
	if mode != "auto" && s.Rotation%2 != 0 {
		w, height = height, w
	}
	encodeW, encodeH := s.launchWidth, s.launchHeight
	if encodeW <= 0 || encodeH <= 0 {
		encodeW, encodeH = s.Width, s.Height
	}
	if err := h.releaseInputLocked(s); err != nil {
		return nil, err
	}
	request := streamResize{streamLaunch: streamLaunch{
		Pkg: s.app.Pkg, Cmp: s.app.Cmp, UserID: s.app.UserID, ShortcutID: s.app.ShortcutID,
		InstanceID: h.instance, Source: 1, Width: encodeW, Height: encodeH,
		DisplayWidth: w, DisplayHeight: height, DisplayScale: 1, DisplayDensity: 160,
		SupportedMediaFormat: 1, SupportWebRTC: 1, SupportCustomDisplay: mode != "auto",
	}, DisplayID: s.DisplayID}
	if err := h.sendCommandLocked(svcAppStream, 17, request); err != nil {
		return nil, err
	}
	s.OrientationPending, s.OrientationError = mode, ""
	s.orientationDeadline = time.Now().Add(10 * time.Second)
	h.publishLocked()
	return map[string]string{"sessionId": s.ID, "orientation": mode, "status": "orientation requested"}, nil
}

func (h *viewerHub) newSessionLocked(mode string, app catalogApp, name string, width, height int) *streamSession {
	h.nextSession++
	s := &streamSession{viewerSession: viewerSession{
		ID: "v" + strconv.FormatUint(h.nextSession, 10), Name: name, Mode: mode, State: "requesting",
		Width: width, Height: height, DisplayID: -1, Orientation: "auto",
	}, app: app, deadline: time.Now().Add(45 * time.Second), launchWidth: width, launchHeight: height}
	h.sessions[s.ID] = s
	h.order = append(h.order, s)
	return s
}

func (h *viewerHub) command(cmd viewerCommand) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.eventsOpen || h.closed {
		return nil, errors.New("phone workspace is not connected")
	}
	if h.control == nil {
		return nil, errors.New("connect the paired phone first")
	}
	switch cmd.Op {
	case "launch":
		if (cmd.Width != 1280 || cmd.Height != 720) && (cmd.Width != 1920 || cmd.Height != 1080) {
			return nil, errors.New("choose a supported display size")
		}
		var app catalogApp
		name := "Mobile desktop"
		switch cmd.Mode {
		case "desktop":
			app.Pkg = "desktop"
		case "app":
			var ok bool
			app, ok = h.catalog[cmd.AppID]
			if !ok {
				return nil, errors.New("app is no longer in the phone's catalog")
			}
			name = app.Name
		default:
			return nil, errors.New("unknown streaming mode")
		}
		for _, s := range h.order {
			if s.app.id() == app.id() && s.State != "failed" {
				return map[string]string{"sessionId": s.ID}, nil
			}
		}
		s := h.newSessionLocked(cmd.Mode, app, name, cmd.Width, cmd.Height)
		var err error
		if cmd.Mode == "desktop" {
			err = h.sendLocked(svcPhoneDesktop, desktopRequest{Ver: "1.0.0", Cmd: 1, View: 2,
				Resolution: &streamResolution{Width: s.Width, Height: s.Height}})
		} else {
			err = h.launchVideoLocked(s)
		}
		if err != nil {
			h.removeSessionLocked(s, false)
			return nil, err
		}
		h.publishLocked()
		return map[string]string{"sessionId": s.ID}, nil
	case "orientation":
		return h.requestOrientationLocked(cmd)
	case "stop":
		if s := h.sessions[cmd.SessionID]; s != nil {
			h.removeSessionLocked(s, true)
			h.publishLocked()
		}
		return nil, nil
	case "stop-audio":
		h.stopAudioLocked()
		h.publishLocked()
		return nil, nil
	case "signal":
		return nil, h.signalLocked(cmd)
	case "input":
		s := h.sessions[cmd.SessionID]
		if s == nil || cmd.Input == nil {
			return nil, errors.New("display is no longer available")
		}
		return nil, h.inputLocked(s, *cmd.Input)
	default:
		return nil, errors.New("unknown workspace command")
	}
}

type streamPhoneData struct {
	SessionID    int64  `json:"sessionId"`
	RequestID    int32  `json:"requestId"`
	Pkg          string `json:"pkg"`
	Cmp          string `json:"cmp"`
	UserID       int    `json:"userId"`
	ShortcutID   string `json:"shortcutId"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	DisplayID    *int   `json:"displayId"`
	State        *int   `json:"state"`
	Error        int    `json:"error"`
	Msg          string `json:"msg"`
	SupportTouch *bool  `json:"supportTouch"`
	IsWebRTC     int    `json:"isWebRTC"`
	WebRTCData   string `json:"webRTCData"`
	Protocol     int    `json:"protocol"`
	ProtocolData string `json:"protocolData"`
	Type         string `json:"type"`
	Data         string `json:"data"`
	Rotation     *int   `json:"rotation"`
	Success      *bool  `json:"success"`
	AppName      string `json:"appName"`
}

func streamFailure(code int, detail string) string {
	reasons := map[int]string{
		145: "phone permission was denied", 147: "phone cancelled the request", 293: "desktop is owned by another device",
		296: "phone has no available video encoder", 305: "phone disabled this capability or has a conflicting display",
		306: "phone administrator disabled this capability", 1537: "streaming requires the phone's owner account",
		3858: "phone display is already in use", 3859: "phone has no available video encoder",
		3860: "app is no longer installed", 3861: "phone reclaimed the video encoder",
	}
	reason := reasons[code]
	if reason == "" {
		reason = "phone rejected the streaming session"
	}
	if code != 0 {
		reason += fmt.Sprintf(" (code %d)", code)
	}
	if detail != "" {
		reason += ": " + detail
	}
	return reason
}

// Service comes from the existing envelope analysis. The phone sends feature
// replies with senderId:1; outbound subsystem routing is not an inbound filter.
// Unrelated wallpaper, notification and file frames are not decoded here.
func (h *viewerHub) inbound(control *liveSession, service int, msg []byte) {
	switch service {
	case svcPhoneDesktop, svcAppStream, svcAppList, svcPhoneAudio:
	default:
		return
	}
	var frame struct {
		Data struct {
			Cmd       *int            `json:"cmd"`
			Data      json.RawMessage `json:"data"`
			Apps      []catalogApp    `json:"apps"`
			AppLimit  int             `json:"appLimit"`
			ErrorCode int             `json:"errorCode"`
			Msg       string          `json:"msg"`
		} `json:"data"`
	}
	if json.Unmarshal(msg, &frame) != nil || frame.Data.Cmd == nil {
		if service == svcPhoneAudio {
			log.Printf("[stream] audio: invalid command envelope")
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.control != control {
		return
	}
	cmd := *frame.Data.Cmd
	if service == svcAppList && cmd == 1 {
		apps := make([]viewerApp, 0, len(frame.Data.Apps))
		catalog := make(map[string]catalogApp, len(frame.Data.Apps))
		for _, a := range frame.Data.Apps {
			if a.Pkg == "" || a.Cmp == "" {
				continue
			}
			if a.Name == "" {
				a.Name = a.Pkg
			}
			id := a.id()
			if _, duplicate := catalog[id]; duplicate {
				continue
			}
			catalog[id] = a
			apps = append(apps, viewerApp{ID: id, Name: a.Name, Package: a.Pkg, UserID: a.UserID})
		}
		sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
		h.apps, h.catalog, h.appLimit = apps, catalog, frame.Data.AppLimit
		log.Printf("[stream] phone catalog: %d apps, limit %d", len(apps), h.appLimit)
		h.publishLocked()
		return
	}
	if service == svcPhoneDesktop {
		switch cmd {
		case 5:
			if s := h.desktopLocked(); s != nil {
				h.removeSessionLocked(s, false)
				h.publishLocked()
			}
		case 0:
			if !h.eventsOpen {
				_ = h.sendLocked(svcPhoneDesktop, desktopRequest{Ver: "1.0.0", Cmd: 1, View: 2, Reply: 1})
				log.Printf("[stream] open the local phone workspace before starting mobile desktop")
				return
			}
			s := h.desktopLocked()
			if s == nil {
				s = h.newSessionLocked("desktop", catalogApp{Pkg: "desktop"}, "Mobile desktop", 1920, 1080)
			}
			if err := h.sendLocked(svcPhoneDesktop, desktopRequest{Ver: "1.0.0", Cmd: 1, View: 2,
				Resolution: &streamResolution{Width: s.Width, Height: s.Height}}); err != nil {
				h.errorLocked(err)
			}
			h.publishLocked()
		case 11:
			if s := h.desktopLocked(); s != nil && frame.Data.ErrorCode != 0 {
				h.failLocked(s, streamFailure(frame.Data.ErrorCode, frame.Data.Msg), false)
			}
		}
		return
	}
	var data streamPhoneData
	if len(frame.Data.Data) == 0 || json.Unmarshal(frame.Data.Data, &data) != nil {
		if service == svcPhoneAudio {
			log.Printf("[stream] audio cmd=%d: invalid command data", cmd)
		}
		return
	}
	if service == svcPhoneAudio {
		h.audioInboundLocked(cmd, data)
		return
	}
	s := h.findSessionLocked(data)
	switch cmd {
	case 2:
		if !h.eventsOpen {
			return
		}
		if s == nil {
			mode, name := "app", data.Pkg
			if data.Pkg == "desktop" {
				mode, name = "desktop", "Mobile desktop"
			}
			if data.Pkg == "" {
				h.errorLocked(errors.New("phone omitted the app identity"))
				return
			}
			app := catalogApp{Pkg: data.Pkg, Cmp: data.Cmp, UserID: data.UserID, ShortcutID: data.ShortcutID}
			if known, ok := h.catalog[app.id()]; ok {
				app, name = known, known.Name
			}
			s = h.newSessionLocked(mode, app, name, 1920, 1080)
		}
		if err := h.launchVideoLocked(s); err != nil {
			h.failLocked(s, err.Error(), true)
		}
		h.publishLocked()
	case 3:
		if s == nil {
			return
		}
		if data.Error != 0 {
			h.failLocked(s, streamFailure(data.Error, data.Msg), false)
			return
		}
		if data.WebRTCData == "" && s.wireID == data.SessionID && s.MediaID != "" {
			_ = h.releaseInputLocked(s)
			if data.Width > 0 {
				s.Width = data.Width
			}
			if data.Height > 0 {
				s.Height = data.Height
			}
			if data.DisplayID != nil {
				s.DisplayID = *data.DisplayID
			}
			if err := h.sendCommandLocked(svcAppStream, 4, struct {
				SessionID int64 `json:"sessionId"`
				Resize    int   `json:"resize"`
			}{s.wireID, 1}); err != nil {
				h.errorLocked(err)
			}
			h.publishLocked()
			return
		}
		if data.SessionID <= 0 || data.IsWebRTC != 1 {
			h.failLocked(s, "phone did not offer a WebRTC video session", true)
			return
		}
		var offer rtcDescription
		if json.Unmarshal([]byte(data.WebRTCData), &offer) != nil || offer.Type != "offer" || offer.SDP == "" {
			h.failLocked(s, "phone supplied an invalid video offer", true)
			return
		}
		_ = h.releaseInputLocked(s)
		s.wireID, s.State, s.DisplayID = data.SessionID, "negotiating", -1
		if data.Width > 0 {
			s.Width = data.Width
		}
		if data.Height > 0 {
			s.Height = data.Height
		}
		h.nextMedia++
		s.MediaID = s.ID + ":" + strconv.FormatUint(h.nextMedia, 10)
		if s.naturalWidth == 0 && s.Width > 0 && s.Height > 0 {
			s.naturalWidth, s.naturalHeight = s.Width, s.Height
		}
		s.deadline = time.Now().Add(45 * time.Second)
		h.publishLocked()
		h.emitLocked(map[string]any{"type": "offer", "mediaId": s.MediaID, "kind": "video", "sessionId": s.ID, "description": offer})
		log.Printf("[stream] %s: video offer, %dx%d", s.ID, s.Width, s.Height)
	case 8:
		if s == nil || data.State == nil {
			return
		}
		switch *data.State {
		case 0:
			if data.Error != 0 {
				h.failLocked(s, streamFailure(data.Error, data.Msg), false)
			} else {
				h.removeSessionLocked(s, false)
				h.publishLocked()
			}
		case 1, 3:
			displayChanged := data.DisplayID != nil && s.DisplayID != *data.DisplayID
			if displayChanged {
				_ = h.releaseInputLocked(s)
				s.DisplayID = *data.DisplayID
			}
			if s.DisplayID < 0 {
				h.failLocked(s, "phone did not return an input display ID", true)
				return
			}
			s.State, s.deadline = "connected", time.Time{}
			if data.SupportTouch != nil {
				s.Touch = *data.SupportTouch
			}
			if displayChanged {
				if err := h.inputDevicesLocked(s); err != nil {
					h.errorLocked(err)
				}
			}
			if h.audio == nil {
				h.startAudioLocked()
			}
			h.publishLocked()
			log.Printf("[stream] %s: phone display %d connected", s.ID, s.DisplayID)
		case 2:
			_ = h.releaseInputLocked(s)
			s.State = "paused"
			h.publishLocked()
		}
	case 20:
		if s != nil && s.MediaID != "" {
			h.remoteCandidateLocked(s.MediaID, data)
		}
	case 12:
		target := h.displayLocked(data.DisplayID)
		if target == nil {
			return
		}
		if s != nil && s != target && s.wireID == 0 {
			h.removeSessionLocked(s, false)
		}
		h.identifyDisplayLocked(target, data)
		h.publishLocked()
		h.emitLocked(map[string]any{"type": "focus", "sessionId": target.ID})
	case 13:
		if target := h.displayLocked(data.DisplayID); target != nil {
			h.identifyDisplayLocked(target, data)
			h.publishLocked()
		}
	case 14:
		if target := h.displayLocked(data.DisplayID); target != nil {
			h.emitLocked(map[string]any{"type": "focus", "sessionId": target.ID})
		}
	case 16:
		if s == nil && data.SessionID == 0 {
			s = h.displayLocked(data.DisplayID)
		}
		if s == nil || data.Rotation == nil || *data.Rotation < 0 || *data.Rotation > 3 {
			return
		}
		_ = h.releaseInputLocked(s)
		// Video and input stay in the encoder's natural raster coordinates.
		// Rotation metadata describes how the receiver presents that raster.
		s.Rotation = *data.Rotation
		h.publishLocked()
	case 26:
		s = h.displayLocked(data.DisplayID)
		if s == nil || s.OrientationPending == "" || data.Success == nil {
			return
		}
		if *data.Success {
			s.Orientation = s.OrientationPending
			s.OrientationError = ""
		} else {
			s.OrientationError = "phone rejected the display orientation resize"
			if data.Error != 0 {
				s.OrientationError += fmt.Sprintf(" (code %d)", data.Error)
			}
		}
		s.OrientationPending = ""
		s.orientationDeadline = time.Time{}
		h.publishLocked()
	case 23:
		if s == nil {
			s = h.displayLocked(data.DisplayID)
		}
		if s == nil || data.State == nil {
			return
		}
		if *data.State == 1 {
			_ = h.releaseInputLocked(s)
			s.State = "paused"
		} else if *data.State == 0 {
			s.State = "connected"
		}
		h.publishLocked()
	}
}

func (h *viewerHub) displayLocked(id *int) *streamSession {
	if id == nil {
		return nil
	}
	for _, s := range h.order {
		if s.DisplayID >= 0 && s.DisplayID == *id {
			return s
		}
	}
	return nil
}

func (h *viewerHub) identifyDisplayLocked(s *streamSession, data streamPhoneData) {
	if s.Mode != "app" || data.Pkg == "" {
		return
	}
	app := catalogApp{Pkg: data.Pkg, Cmp: data.Cmp, UserID: data.UserID, ShortcutID: data.ShortcutID, Name: data.AppName}
	if known, ok := h.catalog[app.id()]; ok {
		app = known
	}
	if app.Name == "" {
		app.Name = app.Pkg
	}
	s.app, s.Name = app, app.Name
}

func (h *viewerHub) desktopLocked() *streamSession {
	for _, s := range h.order {
		if s.Mode == "desktop" && s.State != "failed" {
			return s
		}
	}
	return nil
}

func (h *viewerHub) findSessionLocked(data streamPhoneData) *streamSession {
	if data.SessionID != 0 {
		for _, s := range h.order {
			if s.wireID == data.SessionID {
				return s
			}
		}
	}
	if data.Pkg == "" {
		return nil
	}
	for _, s := range h.order {
		if s.State != "failed" && s.app.Pkg == data.Pkg && s.app.UserID == data.UserID &&
			(data.Cmp == "" || s.app.Cmp == data.Cmp) {
			return s
		}
	}
	return nil
}

type streamWireCandidate struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Label     uint16 `json:"label"`
	Candidate string `json:"candidate"`
}

func (h *viewerHub) remoteCandidateLocked(mediaID string, data streamPhoneData) {
	if data.Type != "candidate" {
		return
	}
	var candidate streamWireCandidate
	if json.Unmarshal([]byte(data.Data), &candidate) != nil || candidate.Candidate == "" {
		return
	}
	h.emitLocked(map[string]any{"type": "candidate", "mediaId": mediaID, "candidate": rtcCandidate{
		Candidate: candidate.Candidate, SDPMid: &candidate.ID, SDPMLineIndex: &candidate.Label,
	}})
}

func (h *viewerHub) signalLocked(cmd viewerCommand) error {
	service, answerCmd, iceCmd := svcAppStream, 4, 20
	var wireID int64
	var session *streamSession
	for _, s := range h.order {
		if s.MediaID != "" && s.MediaID == cmd.MediaID {
			wireID, session = s.wireID, s
			break
		}
	}
	if h.audio != nil && h.audio.ID == cmd.MediaID {
		service, answerCmd, iceCmd, wireID = svcPhoneAudio, 3, 8, h.audio.wireID
	}
	if wireID == 0 {
		return errors.New("media session is no longer available")
	}
	if cmd.Description != nil {
		if cmd.Candidate != nil || cmd.Description.Type != "answer" || cmd.Description.SDP == "" {
			return errors.New("expected one WebRTC answer")
		}
		encoded, err := json.Marshal(cmd.Description)
		if err != nil {
			return err
		}
		if service == svcPhoneAudio {
			err := h.sendCommandLocked(service, answerCmd, struct {
				SessionID    int64  `json:"sessionId"`
				Error        int    `json:"error"`
				Protocol     int    `json:"protocol"`
				ProtocolData string `json:"protocolData"`
			}{SessionID: wireID, Protocol: 1, ProtocolData: string(encoded)})
			log.Printf("[stream] audio cmd=%d session=%d: answer sent=%t", answerCmd, wireID, err == nil)
			return err
		}
		return h.sendCommandLocked(service, answerCmd, struct {
			SessionID  int64  `json:"sessionId"`
			Error      int    `json:"error"`
			Resize     int    `json:"resize"`
			IsWebRTC   int    `json:"isWebRTC"`
			WebRTCData string `json:"webRTCData"`
		}{SessionID: wireID, IsWebRTC: 1, WebRTCData: string(encoded)})
	}
	if cmd.Candidate == nil || cmd.Candidate.Candidate == "" {
		return errors.New("expected a WebRTC candidate")
	}
	candidate := streamWireCandidate{Type: "candidate", Candidate: cmd.Candidate.Candidate}
	if cmd.Candidate.SDPMid != nil {
		candidate.ID = *cmd.Candidate.SDPMid
	}
	if cmd.Candidate.SDPMLineIndex != nil {
		candidate.Label = *cmd.Candidate.SDPMLineIndex
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	data := struct {
		SessionID int64  `json:"sessionId"`
		Pkg       string `json:"pkg,omitempty"`
		Cmp       string `json:"cmp,omitempty"`
		UserID    int    `json:"userId"`
		Type      string `json:"type"`
		Data      string `json:"data"`
	}{SessionID: wireID, Type: "candidate", Data: string(encoded)}
	if session != nil {
		data.Pkg, data.Cmp, data.UserID = session.app.Pkg, session.app.Cmp, session.app.UserID
	}
	return h.sendCommandLocked(service, iceCmd, data)
}

func (h *viewerHub) startAudioLocked() {
	h.nextAudio++
	if h.nextAudio <= 0 {
		h.nextAudio = 1
	}
	h.nextMedia++
	h.audio = &audioSession{viewerAudio: viewerAudio{ID: "a" + strconv.FormatUint(h.nextMedia, 10), State: "requesting"},
		requestID: h.nextAudio, deadline: time.Now().Add(30 * time.Second)}
	err := h.sendCommandLocked(svcPhoneAudio, 1, struct {
		InstanceID int32 `json:"instanceId"`
		RequestID  int32 `json:"requestId"`
		Port       int   `json:"port"`
		Protocol   int   `json:"protocol"`
		Freq       int   `json:"freq"`
		Channels   int   `json:"channels"`
		Encoding   int   `json:"encoding"`
	}{InstanceID: h.instance, RequestID: h.audio.requestID, Protocol: 1, Freq: 48000, Channels: 2, Encoding: 16})
	log.Printf("[stream] audio cmd=1 request=%d protocol=1 freq=48000 channels=2 encoding=16: requested sent=%t", h.audio.requestID, err == nil)
	if err != nil {
		h.audio.State, h.audio.Error = "failed", err.Error()
		h.errorLocked(err)
	}
}

func (h *viewerHub) audioInboundLocked(cmd int, data streamPhoneData) {
	// Log scalar lifecycle fields only: SDP, ICE addresses and protocol data may
	// contain private network information or credentials.
	switch cmd {
	case 4:
		if data.State == nil {
			log.Printf("[stream] audio cmd=4 session=%d: missing state", data.SessionID)
		} else {
			log.Printf("[stream] audio cmd=4 session=%d state=%d active=%t", data.SessionID, *data.State, h.audio != nil && data.SessionID == h.audio.wireID)
		}
	case 8:
		// ICE candidates are frequent and are never logged.
	default:
		log.Printf("[stream] audio cmd=%d request=%d session=%d protocol=%d error=%d", cmd, data.RequestID, data.SessionID, data.Protocol, data.Error)
	}
	if cmd == 1 {
		// Reverse microphone capture is intentionally not enabled. A nonzero
		// command-2 error declines it without stopping phone-output audio.
		err := h.sendCommandLocked(svcPhoneAudio, 2, struct {
			RequestID int32 `json:"requestId"`
			Error     int   `json:"error"`
		}{data.RequestID, 1})
		log.Printf("[stream] audio cmd=2 request=%d error=1: microphone declined sent=%t", data.RequestID, err == nil)
		return
	}
	a := h.audio
	if a == nil {
		return
	}
	switch cmd {
	case 2:
		if data.RequestID != a.requestID {
			log.Printf("[stream] audio cmd=2 request=%d: ignored; active request=%d", data.RequestID, a.requestID)
			return
		}
		if data.Error != 0 || data.Protocol != 1 || data.SessionID <= 0 {
			h.failAudioLocked(streamFailure(data.Error, data.Msg))
			return
		}
		var offer rtcDescription
		if json.Unmarshal([]byte(data.ProtocolData), &offer) != nil || offer.Type != "offer" || offer.SDP == "" {
			h.failAudioLocked("phone supplied an invalid audio offer")
			return
		}
		a.wireID, a.State = data.SessionID, "negotiating"
		log.Printf("[stream] audio request=%d session=%d: offer accepted", a.requestID, a.wireID)
		h.publishLocked()
		h.emitLocked(map[string]any{"type": "offer", "mediaId": a.ID, "kind": "audio", "sessionId": "", "description": offer})
	case 4:
		if data.SessionID != a.wireID || data.State == nil {
			return
		}
		switch *data.State {
		case 0:
			a.State, a.ID, a.deadline = "stopped", "", time.Time{}
		case 1, 3:
			a.State, a.deadline = "connected", time.Time{}
		case 2:
			a.State = "paused"
		}
		h.publishLocked()
	case 8:
		if data.SessionID == a.wireID {
			h.remoteCandidateLocked(a.ID, data)
		}
	}
}

func (h *viewerHub) hasRunningLocked() bool {
	for _, s := range h.order {
		if s.State != "failed" {
			return true
		}
	}
	return false
}

func (h *viewerHub) stopAudioLocked() {
	if h.audio == nil {
		return
	}
	if h.control != nil && h.audio.ID != "" {
		if err := h.sendCommandLocked(svcPhoneAudio, 5, struct {
			SessionID int64 `json:"sessionId"`
			RequestID int32 `json:"requestId"`
		}{
			h.audio.wireID, h.audio.requestID}); err != nil {
			log.Printf("[stream] audio cmd=5 request=%d session=%d: stop sent=false", h.audio.requestID, h.audio.wireID)
			log.Printf("[stream] stop audio: %v", err)
		} else {
			log.Printf("[stream] audio cmd=5 request=%d session=%d: stop sent=true", h.audio.requestID, h.audio.wireID)
		}
	}
	h.audio = nil
}

func (h *viewerHub) failAudioLocked(message string) {
	a := h.audio
	if a == nil {
		return
	}
	log.Printf("[stream] audio request=%d session=%d state=%s: failed", a.requestID, a.wireID, a.State)
	h.stopAudioLocked()
	a.State, a.ID, a.Error, a.wireID, a.deadline = "failed", "", message, 0, time.Time{}
	h.audio = a
	h.publishLocked()
}

func (h *viewerHub) stopRemoteLocked(s *streamSession) {
	_ = h.releaseInputLocked(s)
	if h.control == nil {
		return
	}
	if s.wireID != 0 {
		if err := h.sendCommandLocked(svcAppStream, 7, struct {
			SessionID int64 `json:"sessionId"`
			Error     int   `json:"error"`
		}{SessionID: s.wireID}); err != nil {
			log.Printf("[stream] stop video: %v", err)
		}
	}
	if s.Mode == "desktop" {
		if err := h.sendLocked(svcPhoneDesktop, desktopRequest{Ver: "1.0.0", Cmd: 5}); err != nil {
			log.Printf("[stream] stop desktop: %v", err)
		}
	}
}

func (h *viewerHub) removeSessionLocked(s *streamSession, stopRemote bool) {
	if stopRemote {
		h.stopRemoteLocked(s)
	}
	delete(h.sessions, s.ID)
	for i, current := range h.order {
		if current == s {
			copy(h.order[i:], h.order[i+1:])
			h.order[len(h.order)-1] = nil
			h.order = h.order[:len(h.order)-1]
			break
		}
	}
	if !h.hasRunningLocked() {
		h.stopAudioLocked()
	}
}

func (h *viewerHub) failLocked(s *streamSession, message string, stopRemote bool) {
	if stopRemote {
		h.stopRemoteLocked(s)
	}
	s.State, s.Error, s.MediaID, s.DisplayID, s.wireID = "failed", message, "", -1, 0
	s.keys, s.buttons, s.touches, s.deadline = nil, nil, nil, time.Time{}
	if !h.hasRunningLocked() {
		h.stopAudioLocked()
	}
	h.errorLocked(errors.New(s.Name + ": " + message))
	h.publishLocked()
}

func (h *viewerHub) stopAllLocked() {
	for _, s := range h.order {
		h.stopRemoteLocked(s)
	}
	h.sessions, h.order = make(map[string]*streamSession), nil
	h.stopAudioLocked()
}

func (h *viewerHub) maintain() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	lastHeartbeat := time.Time{}
	for {
		select {
		case <-h.done:
			return
		case now := <-ticker.C:
			var heartbeat bool
			h.mu.Lock()
			for _, s := range h.order {
				if !s.deadline.IsZero() && !now.Before(s.deadline) {
					h.failLocked(s, "phone did not finish starting this display within 45 seconds; check phone consent", true)
				}
				if s.OrientationPending != "" && !now.Before(s.orientationDeadline) {
					s.OrientationPending = ""
					s.orientationDeadline = time.Time{}
					s.OrientationError = "phone did not acknowledge the orientation request"
					h.publishLocked()
				}
			}
			if h.audio != nil && !h.audio.deadline.IsZero() && !now.Before(h.audio.deadline) {
				h.failAudioLocked("phone audio did not become ready; check phone permission")
			}
			if h.control != nil && h.hasRunningLocked() && now.Sub(lastHeartbeat) >= 20*time.Second {
				heartbeat = true
				lastHeartbeat = now
			}
			h.mu.Unlock()
			if heartbeat {
				if err := h.sendCommand(svcAppStream, 9, nil); err != nil {
					h.mu.Lock()
					h.errorLocked(err)
					h.mu.Unlock()
				}
			}
		}
	}
}
