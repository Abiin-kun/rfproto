package main

/*
#cgo pkg-config: sdl3 freetype2
#cgo LDFLAGS: -lm
#include "native_ui.h"
#include <stdlib.h>
*/
import "C"

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"
)

//go:embed web/InterVariable.woff2
var nativeUIFont []byte

type nativeRect struct{ x, y, w, h float32 }

func (r nativeRect) contains(x, y float32) bool {
	return x >= r.x && y >= r.y && x < r.x+r.w && y < r.y+r.h
}

type nativeWidget struct {
	id, label, action, value string
	subtitle                 string
	rect                     nativeRect
	enabled                  bool
	region                   string
}
type nativeTextKey struct {
	text string
	size int
}
type nativeCachedText struct {
	texture *C.native_text
	used    uint64
}
type nativeAppRow struct {
	app viewerApp
	id  string
}
type nativeSessionTab struct {
	session             viewerSession
	id, detail, waiting string
}
type nativeTouchKey struct{ device, finger int64 }
type nativeTouch struct{ id, x, y int }
type nativeWorkspace struct {
	hub                                                                *viewerHub
	media                                                              *nativeMedia
	window                                                             *C.native_ui
	state                                                              viewerState
	active, preferred, focus, search, errorText, composition, appCount string
	apps                                                               []nativeAppRow
	tabs                                                               []nativeSessionTab
	widgets                                                            []nativeWidget
	texts                                                              map[nativeTextKey]*nativeCachedText
	keys                                                               map[int]int
	touches                                                            map[nativeTouchKey]nativeTouch
	buttons                                                            map[int]bool
	frame                                                              uint64
	density                                                            float32
	width, height                                                      int
	scroll, tabScroll                                                  float32
	mouseX, mouseY                                                     float32
	list, tabArea, display                                             nativeRect
	large, muted, fullscreen                                           bool
	textEnabled                                                        bool
	textArea                                                           nativeRect
	textFailure                                                        error
}

// runNativeViewer must run on the OS-locked main thread. SDL chooses its native
// video backend, including Wayland, and honors SDL_VIDEODRIVER overrides.
func runNativeViewer(h *viewerHub, stop <-chan os.Signal) error {
	window := C.nui_open(unsafe.Pointer(&nativeUIFont[0]), C.size_t(len(nativeUIFont)))
	if window == nil {
		return fmt.Errorf("native workspace: %s", C.GoString(C.SDL_GetError()))
	}
	log.Printf("[ui] SDL phone workspace ready (video=%s)", C.GoString(C.SDL_GetCurrentVideoDriver()))
	defer C.nui_close(window)
	media := newNativeMedia(h)
	defer media.Close()
	events, unsubscribe, err := h.subscribe()
	if err != nil {
		return err
	}
	// Stop remote resources before media callbacks and SDL are torn down.
	defer unsubscribe()
	u := &nativeWorkspace{hub: h, media: media, window: window, focus: "search",
		texts: make(map[nativeTextKey]*nativeCachedText), keys: make(map[int]int),
		touches: make(map[nativeTouchKey]nativeTouch), buttons: make(map[int]bool)}
	defer u.clearTexts()
	defer u.release()
	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()
	closing := false
	for {
		// Bound event work per frame without ever blocking on an idle window.
		for n := 0; n < 512; n++ {
			select {
			case event, ok := <-events:
				if !ok {
					return fmt.Errorf("phone workspace event subscription closed")
				}
				media.Handle(event)
				u.handleHub(event)
			default:
				n = 512
			}
		}
		var w, height C.int
		density := float32(C.nui_size(window, &w, &height))
		if density != u.density {
			u.clearTexts()
			u.density = density
		}
		u.width, u.height = int(w), int(height)
		u.layout()
		var event C.native_event
		for range 512 {
			if C.nui_poll(&event) == 0 {
				break
			}
			if event.kind == C.NUI_QUIT {
				closing = true
				break
			}
			u.handleInput(event)
		}
		select {
		case <-stop:
			closing = true
		default:
		}
		// Re-layout after search, focus and fullscreen actions from this batch.
		u.layout()
		u.draw()
		if u.textFailure != nil {
			return u.textFailure
		}
		C.nui_present(window)
		if closing {
			return nil
		}
		select {
		case <-stop:
			closing = true
		case <-tick.C:
		}
	}
}

func (u *nativeWorkspace) clearTexts() {
	for key, text := range u.texts {
		C.nui_text_free(text.texture)
		delete(u.texts, key)
	}
}
func (u *nativeWorkspace) label(text string, size int, x, y float32, color uint32) {
	if text == "" {
		return
	}
	key := nativeTextKey{text, size}
	cached := u.texts[key]
	if cached == nil {
		// Only string/size changes rasterize; stable labels reuse SDL textures.
		if len(u.texts) >= 512 {
			var oldest nativeTextKey
			age := uint64(^uint64(0))
			for k, t := range u.texts {
				if t.used < age {
					oldest, age = k, t.used
				}
			}
			C.nui_text_free(u.texts[oldest].texture)
			delete(u.texts, oldest)
		}
		ctext := C.CString(text)
		texture := C.nui_text_new(u.window, ctext, C.int(size), C.float(u.density))
		C.free(unsafe.Pointer(ctext))
		if texture == nil {
			u.textFailure = fmt.Errorf("workspace text: %s", C.GoString(C.SDL_GetError()))
			return
		}
		cached = &nativeCachedText{texture: texture}
		u.texts[key] = cached
	}
	cached.used = u.frame
	C.nui_text_draw(u.window, cached.texture, C.float(x), C.float(y), C.uint(color))
}
func (u *nativeWorkspace) fill(r nativeRect, color uint32) {
	C.nui_fill(u.window, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.uint(color))
}
func (u *nativeWorkspace) outline(r nativeRect, color uint32) {
	C.nui_outline(u.window, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.uint(color))
}
func (u *nativeWorkspace) clip(r nativeRect) {
	C.nui_clip(u.window, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h))
}
func (u *nativeWorkspace) current() *viewerSession {
	for i := range u.state.Sessions {
		if u.state.Sessions[i].ID == u.active {
			return &u.state.Sessions[i]
		}
	}
	return nil
}
func (u *nativeWorkspace) ready() *viewerSession {
	s := u.current()
	if s == nil || !u.state.Connected || s.State != "connected" || s.DisplayID < 0 {
		return nil
	}
	return s
}
func (u *nativeWorkspace) command(cmd viewerCommand) any {
	result, err := u.hub.command(cmd)
	if err != nil {
		u.errorText = err.Error()
		return nil
	}
	return result
}
func (u *nativeWorkspace) input(in viewerInput) {
	if u.ready() != nil {
		u.command(viewerCommand{Op: "input", SessionID: u.active, Input: &in})
	}
}
func (u *nativeWorkspace) release() {
	if u.active != "" && u.state.Connected && (len(u.keys) != 0 || len(u.buttons) != 0 || len(u.touches) != 0) {
		u.command(viewerCommand{Op: "input", SessionID: u.active, Input: &viewerInput{Type: "release"}})
	}
	u.forgetInput()
}

// Authoritative phone state already released or destroyed remote input devices.
// Forget local tracking without sending a second release to a removed session.
func (u *nativeWorkspace) forgetInput() {
	clear(u.keys)
	clear(u.buttons)
	clear(u.touches)
	u.composition = ""
	if u.window != nil {
		C.nui_capture(0)
	}
}
func (u *nativeWorkspace) setFocus(id string) {
	if id != u.focus {
		if u.focus == "display" {
			u.release()
		}
		u.focus = id
		u.composition = ""
	}
}
func (u *nativeWorkspace) loseFocus() { u.release(); u.focus = "" }
func (u *nativeWorkspace) selectSession(id string) {
	if id == u.active {
		return
	}
	u.release()
	u.active = id
	for i, tab := range u.tabs {
		if tab.session.ID != id {
			continue
		}
		left := float32(i) * 190
		if left < u.tabScroll {
			u.tabScroll = left
		} else if left+184 > u.tabScroll+u.tabArea.w {
			u.tabScroll = max(float32(0), left+184-u.tabArea.w)
		}
		break
	}
}
func (u *nativeWorkspace) filterApps() {
	u.apps = u.apps[:0]
	query := strings.ToLower(strings.TrimSpace(u.search))
	for _, app := range u.state.Apps {
		if query == "" || strings.Contains(strings.ToLower(app.Name), query) || strings.Contains(strings.ToLower(app.Package), query) {
			u.apps = append(u.apps, nativeAppRow{app: app, id: "app:" + app.ID})
		}
	}
	u.scroll = 0
	u.appCount = fmt.Sprintf("%d apps", len(u.apps))
}
func (u *nativeWorkspace) handleHub(event any) {
	m, ok := event.(map[string]any)
	if !ok {
		return
	}
	switch m["type"] {
	case "state":
		state, ok := m["state"].(viewerState)
		if !ok {
			return
		}
		catalogChanged := len(u.state.Apps) != len(state.Apps) || len(state.Apps) > 0 && &u.state.Apps[0] != &state.Apps[0]
		old := u.current()
		if old != nil {
			found := false
			for _, s := range state.Sessions {
				if s.ID == u.active && s.DisplayID == old.DisplayID && s.MediaID == old.MediaID && s.State == "connected" {
					found = true
					break
				}
			}
			if !found {
				u.forgetInput()
			}
		}
		u.state = state
		if catalogChanged || u.appCount == "" {
			u.filterApps()
		}
		u.tabs = u.tabs[:0]
		for _, s := range state.Sessions {
			detail := fmt.Sprintf("%d × %d · %s", s.Width, s.Height, s.State)
			if s.Touch {
				detail += " · Touch supported"
			}
			u.tabs = append(u.tabs, nativeSessionTab{session: s, id: "session:" + s.ID, detail: detail, waiting: "Waiting for the phone display · " + s.State})
		}
		wanted := u.active
		if u.preferred != "" {
			wanted = u.preferred
		}
		found := false
		for _, s := range state.Sessions {
			if s.ID == wanted {
				found = true
				break
			}
		}
		if found {
			u.selectSession(wanted)
			u.preferred = ""
		} else if u.current() == nil {
			id := ""
			if len(state.Sessions) > 0 {
				id = state.Sessions[len(state.Sessions)-1].ID
			}
			u.selectSession(id)
		}
		if !state.Connected {
			u.errorText = ""
			u.setFocus("search")
		}
	case "focus":
		id, _ := m["sessionId"].(string)
		u.preferred = id
		for _, s := range u.state.Sessions {
			if s.ID == id {
				u.selectSession(id)
				u.preferred = ""
				break
			}
		}
	case "error":
		u.errorText, _ = m["message"].(string)
	}
}
func (u *nativeWorkspace) add(id, label, action, value, region string, r nativeRect, enabled bool) {
	u.widgets = append(u.widgets, nativeWidget{id: id, label: label, action: action, value: value, region: region, rect: r, enabled: enabled})
}
func (u *nativeWorkspace) layout() {
	w, h := float32(u.width), float32(u.height)
	side := float32(280)
	if w < 1050 {
		side = 240
	}
	right := side + 24
	u.list = nativeRect{16, 252, side - 32, max(float32(0), h-320)}
	maxScroll := max(float32(0), float32(len(u.apps))*58-u.list.h)
	u.scroll = max(float32(0), min(u.scroll, maxScroll))
	u.tabArea = nativeRect{right, 90, w - right - 24, 42}
	u.tabScroll = max(float32(0), min(u.tabScroll, max(float32(0), float32(len(u.tabs))*190-u.tabArea.w)))
	u.widgets = u.widgets[:0]
	u.add("desktop", "Open mobile desktop", "desktop", "", "", nativeRect{16, 94, side - 32, 40}, u.state.Connected)
	u.add("size720", "1280 × 720", "size720", "", "", nativeRect{16, 148, (side - 40) / 2, 34}, true)
	u.add("size1080", "1920 × 1080", "size1080", "", "", nativeRect{24 + (side-40)/2, 148, (side - 40) / 2, 34}, true)
	u.add("search", "", "search", "", "", nativeRect{16, 210, side - 32, 34}, true)
	for i, row := range u.apps {
		u.add(row.id, row.app.Name, "app", row.app.ID, "apps", nativeRect{16, u.list.y + float32(i)*58 - u.scroll, side - 32, 56}, u.state.Connected)
		u.widgets[len(u.widgets)-1].subtitle = row.app.Package
	}
	for i, tab := range u.tabs {
		u.add(tab.id, tab.session.Name, "session", tab.session.ID, "tabs", nativeRect{right + float32(i)*190 - u.tabScroll, 90, 184, 40}, true)
	}
	mute := "Mute"
	if u.muted {
		mute = "Unmute"
	}
	fullscreen := "Fullscreen"
	if u.fullscreen {
		fullscreen = "Exit fullscreen"
	}
	u.add("mute", mute, "mute", "", "", nativeRect{right, 148, 90, 34}, true)
	u.add("fullscreen", fullscreen, "fullscreen", "", "", nativeRect{right + 98, 148, 140, 34}, true)
	u.add("close", "Close session", "close", "", "", nativeRect{right + 246, 148, 130, 34}, u.current() != nil)
	area := nativeRect{right, 200, w - right - 24, max(float32(0), h-292)}
	u.display = area
	if s := u.current(); s != nil && s.Width > 0 && s.Height > 0 {
		scale := min(area.w/float32(s.Width), area.h/float32(s.Height))
		u.display.w = float32(s.Width) * scale
		u.display.h = float32(s.Height) * scale
		u.display.x += (area.w - u.display.w) / 2
		u.display.y += (area.h - u.display.h) / 2
	}
	u.add("display", "", "display", "", "", u.display, u.ready() != nil)
	enabled := u.focus == "search" || (u.focus == "display" && u.ready() != nil)
	rect := u.display
	if u.focus == "search" {
		rect = nativeRect{16, 210, side - 32, 34}
	}
	if enabled != u.textEnabled || rect != u.textArea {
		on := C.int(0)
		if enabled {
			on = 1
		}
		C.nui_text_input(u.window, on, C.float(rect.x), C.float(rect.y), C.float(rect.w), C.float(rect.h))
		u.textEnabled, u.textArea = enabled, rect
	}
}
func (u *nativeWorkspace) draw() {
	u.frame++
	C.nui_begin(u.window)
	w, h := float32(u.width), float32(u.height)
	side := u.list.w + 32
	u.fill(nativeRect{0, 0, side, h}, 0x171717)
	u.label("rfproto", 24, 20, 20, 0xf3f3f3)
	phone := "Phone disconnected"
	color := uint32(0xb4b4b4)
	if u.state.Connected {
		phone = u.state.Phone
		if phone == "" {
			phone = "Authenticated phone"
		}
		color = 0x71c292
	}
	u.clip(nativeRect{side + 24, 24, w - side - 48, 30})
	u.label(phone, 16, side+24, 28, color)
	C.nui_unclip(u.window)
	u.label("Phone workspace", 14, 20, 53, 0xb4b4b4)
	u.label("Search apps", 14, 18, 188, 0xb4b4b4)
	for _, widget := range u.widgets {
		if widget.id == "display" {
			continue
		}
		if widget.region == "apps" {
			if widget.rect.y+widget.rect.h <= u.list.y || widget.rect.y >= u.list.y+u.list.h {
				continue
			}
			u.clip(u.list)
		} else if widget.region == "tabs" {
			if widget.rect.x+widget.rect.w <= u.tabArea.x || widget.rect.x >= u.tabArea.x+u.tabArea.w {
				continue
			}
			u.clip(u.tabArea)
		}
		bg := uint32(0x252525)
		if widget.region == "apps" {
			bg = 0x171717
		}
		if widget.action == "session" && widget.value == u.active {
			bg = 0x383838
		}
		if (widget.id == "size720" && !u.large) || (widget.id == "size1080" && u.large) {
			bg = 0x414141
		}
		if widget.enabled && widget.rect.contains(u.mouseX, u.mouseY) {
			bg = 0x333333
		}
		u.fill(widget.rect, bg)
		if u.focus == widget.id {
			u.outline(widget.rect, 0xdddddd)
		}
		color := uint32(0xeeeeee)
		if !widget.enabled {
			color = 0x949494
		}
		inner := widget.rect
		inner.x += 7
		inner.w -= 14
		// Intersect label clipping with its scroll region, never the adjacent pane.
		if widget.region == "apps" {
			y := max(inner.y, u.list.y)
			inner.h = min(inner.y+inner.h, u.list.y+u.list.h) - y
			inner.y = y
		}
		if widget.region == "tabs" {
			x := max(inner.x, u.tabArea.x)
			inner.w = min(inner.x+inner.w, u.tabArea.x+u.tabArea.w) - x
			inner.x = x
		}
		u.clip(inner)
		if widget.id == "search" {
			text := u.search
			if text == "" {
				text = "App name or package"
				color = 0x949494
			}
			u.label(text, 14, widget.rect.x+8, widget.rect.y+6, color)
		} else {
			y := widget.rect.y + (widget.rect.h-22)/2
			if widget.region == "apps" {
				y = widget.rect.y + 5
			}
			u.label(widget.label, 14, widget.rect.x+8, y, color)
			if widget.region == "apps" {
				u.label(widget.subtitle, 12, widget.rect.x+8, widget.rect.y+29, 0x949494)
			}
		}
		C.nui_unclip(u.window)
	}
	if len(u.apps) == 0 {
		message := "No matching apps"
		if !u.state.Connected {
			message = "Connect the paired phone"
		} else if len(u.state.Apps) == 0 {
			message = "Waiting for the phone's app catalog"
		}
		u.clip(u.list)
		u.label(message, 13, u.list.x+4, u.list.y+14, 0xb4b4b4)
		C.nui_unclip(u.window)
	}
	u.label(u.appCount, 13, 18, h-55, 0x949494)
	u.fill(u.display, 0x080808)
	s := u.current()
	message := "Choose an app or open mobile desktop"
	if !u.state.Connected {
		message = "Waiting for an authenticated phone"
	} else if s != nil {
		switch {
		case s.Error != "":
			message = s.Error
		case s.State == "paused":
			message = "Paused by the phone"
		case s.State == "failed":
			message = "Phone session failed"
		case s.State == "connected":
			if err := u.media.Draw(s.MediaID, unsafe.Pointer(C.nui_renderer(u.window)), u.display.x, u.display.y, u.display.w, u.display.h); err != nil {
				message = err.Error()
			} else {
				message = ""
			}
		default:
			message = "Waiting for the phone display"
			for _, tab := range u.tabs {
				if tab.session.ID == s.ID {
					message = tab.waiting
					break
				}
			}
		}
	}
	if message != "" {
		u.clip(u.display)
		u.label(message, 16, u.display.x+20, u.display.y+u.display.h/2-14, 0xb4b4b4)
		C.nui_unclip(u.window)
	}
	if u.focus == "display" {
		u.outline(u.display, 0xdddddd)
	}
	right := side + 24
	u.clip(nativeRect{right, h - 80, w - right - 24, 26})
	if s != nil {
		for _, tab := range u.tabs {
			if tab.session.ID == s.ID {
				u.label(tab.detail, 13, right, h-78, 0xb4b4b4)
				break
			}
		}
	}
	C.nui_unclip(u.window)
	status := "Tab: controls · Click display to type · Esc: release focus"
	color = 0x949494
	if u.errorText != "" {
		status = u.errorText
		color = 0xf0a1a1
	} else if u.state.Audio.Error != "" {
		status = u.state.Audio.Error
		color = 0xf0a1a1
	} else if u.composition != "" {
		status = u.composition
		color = 0xeeeeee
	}
	u.clip(nativeRect{right, h - 51, w - right - 24, 42})
	u.label(status, 13, right, h-49, color)
	C.nui_unclip(u.window)
}
func (u *nativeWorkspace) activate(widget nativeWidget) {
	if !widget.enabled {
		return
	}
	u.setFocus(widget.id)
	switch widget.action {
	case "search", "display":
	case "size720":
		u.large = false
	case "size1080":
		u.large = true
	case "session":
		u.selectSession(widget.value)
	case "mute":
		u.muted = !u.muted
		u.media.SetMuted(u.muted)
	case "fullscreen":
		enabled := C.int(1)
		if u.fullscreen {
			enabled = 0
		}
		if C.nui_fullscreen(u.window, enabled) == 0 {
			u.errorText = C.GoString(C.SDL_GetError())
		} else {
			u.fullscreen = !u.fullscreen
		}
	case "close":
		u.release()
		u.command(viewerCommand{Op: "stop", SessionID: u.active})
	case "desktop", "app":
		width, height := 1280, 720
		if u.large {
			width, height = 1920, 1080
		}
		mode := "desktop"
		if widget.action == "app" {
			mode = "app"
		}
		result := u.command(viewerCommand{Op: "launch", Mode: mode, AppID: widget.value, Width: width, Height: height})
		if reply, ok := result.(map[string]string); ok {
			u.preferred = reply["sessionId"]
			for _, s := range u.state.Sessions {
				if s.ID == u.preferred {
					u.selectSession(s.ID)
					u.preferred = ""
					break
				}
			}
		}
	}
}
func (u *nativeWorkspace) traverse(back bool) {
	index := -1
	for i, w := range u.widgets {
		if w.id == u.focus {
			index = i
			break
		}
	}
	step := 1
	if back {
		step = -1
	}
	for range len(u.widgets) {
		index = (index + step + len(u.widgets)) % len(u.widgets)
		widget := u.widgets[index]
		if !widget.enabled {
			continue
		}
		u.setFocus(widget.id)
		if widget.region == "apps" {
			if widget.rect.y < u.list.y {
				u.scroll -= u.list.y - widget.rect.y
			} else if widget.rect.y+widget.rect.h > u.list.y+u.list.h {
				u.scroll += widget.rect.y + widget.rect.h - u.list.y - u.list.h
			}
		}
		if widget.region == "tabs" {
			if widget.rect.x < u.tabArea.x {
				u.tabScroll -= u.tabArea.x - widget.rect.x
			} else if widget.rect.x+widget.rect.w > u.tabArea.x+u.tabArea.w {
				u.tabScroll += widget.rect.x + widget.rect.w - u.tabArea.x - u.tabArea.w
			}
		}
		return
	}
}

// point is the only window-to-phone coordinate transform. Captured gestures
// clamp to the picture; an initial press in a letterbox is ignored.
func (u *nativeWorkspace) point(x, y float32, clamp bool) (int, int, bool) {
	s := u.ready()
	r := u.display
	if s == nil || r.w <= 0 || r.h <= 0 || (!clamp && !r.contains(x, y)) {
		return 0, 0, false
	}
	px := int((x - r.x) * float32(s.Width) / r.w)
	py := int((y - r.y) * float32(s.Height) / r.h)
	return max(0, min(s.Width-1, px)), max(0, min(s.Height-1, py)), true
}
func (u *nativeWorkspace) handleInput(e C.native_event) {
	x, y := float32(e.x), float32(e.y)
	switch e.kind {
	case C.NUI_BLUR:
		u.loseFocus()
	case C.NUI_KEY:
		u.keyboard(e)
	case C.NUI_EDIT:
		if u.focus == "display" {
			u.release()
		}
		if u.focus == "display" || u.focus == "search" {
			u.composition = C.GoString(e.text)
		}
	case C.NUI_TEXT:
		text := C.GoString(e.text)
		u.composition = ""
		if u.focus == "search" {
			if len(u.search)+len(text) <= 1024 {
				u.search += text
				u.filterApps()
			}
		} else if u.focus == "display" {
			u.input(viewerInput{Type: "text", Text: text})
		}
	case C.NUI_MOVE:
		u.mouseX, u.mouseY = x, y
		px, py, ok := u.point(x, y, len(u.buttons) > 0)
		if ok {
			u.input(viewerInput{Type: "move", X: px, Y: py})
		}
	case C.NUI_BUTTON:
		key := int(e.button)
		if key < 1 || key > 3 {
			return
		}
		if e.down != 0 {
			if key == 1 {
				for _, widget := range u.widgets {
					if widget.region == "apps" && !u.list.contains(x, y) {
						continue
					}
					if widget.region == "tabs" && !u.tabArea.contains(x, y) {
						continue
					}
					if widget.rect.contains(x, y) && widget.id != "display" {
						u.activate(widget)
						return
					}
				}
			}
			px, py, ok := u.point(x, y, false)
			if !ok {
				return
			}
			u.setFocus("display")
			u.buttons[key] = true
			C.nui_capture(1)
			u.input(viewerInput{Type: "button", Key: key, Down: true, X: px, Y: py})
		} else if u.buttons[key] {
			px, py, ok := u.point(x, y, true)
			if ok {
				u.input(viewerInput{Type: "button", Key: key, X: px, Y: py})
			}
			delete(u.buttons, key)
			if len(u.buttons) == 0 {
				C.nui_capture(0)
			}
		}
	case C.NUI_WHEEL:
		if u.list.contains(x, y) {
			u.scroll -= float32(e.wheel) * 58
			return
		}
		if u.tabArea.contains(x, y) {
			u.tabScroll -= float32(e.wheel) * 190
			return
		}
		px, py, ok := u.point(x, y, false)
		if ok && e.wheel != 0 {
			u.input(viewerInput{Type: "wheel", X: px, Y: py, Down: e.wheel < 0})
		}
	case C.NUI_TOUCH:
		u.touch(e)
	}
}
func (u *nativeWorkspace) touch(e C.native_event) {
	key := nativeTouchKey{int64(e.device), int64(e.finger)}
	held, exists := u.touches[key]
	phase := int(e.phase)
	x, y := float32(e.x)*float32(u.width), float32(e.y)*float32(u.height)
	if phase == 1 {
		for _, widget := range u.widgets {
			if widget.region == "apps" && !u.list.contains(x, y) {
				continue
			}
			if widget.region == "tabs" && !u.tabArea.contains(x, y) {
				continue
			}
			if widget.id != "display" && widget.rect.contains(x, y) {
				u.activate(widget)
				return
			}
		}
	}
	s := u.ready()
	if s == nil {
		return
	}
	px, py, ok := u.point(x, y, exists)
	if !ok {
		return
	}
	if phase == 1 {
		if !s.Touch && len(u.touches) > 0 {
			return
		}
		id := -1
		for n := range 32 {
			used := false
			for _, t := range u.touches {
				if t.id == n {
					used = true
					break
				}
			}
			if !used {
				id = n
				break
			}
		}
		if id < 0 {
			return
		}
		held = nativeTouch{id: id}
		u.setFocus("display")
		exists = true
	}
	if !exists {
		return
	}
	held.x, held.y = px, py
	if s.Touch {
		pressure := float64(e.pressure)
		if phase == 0 || phase == 3 {
			pressure = 0
		} else if pressure == 0 {
			pressure = 0.5
		}
		u.input(viewerInput{Type: "touch", Event: phase, PointerID: held.id, X: px, Y: py, Pressure: pressure, MajorAxisSize: 1})
	} else if phase == 2 {
		u.input(viewerInput{Type: "move", X: px, Y: py})
	} else {
		u.input(viewerInput{Type: "button", Key: 1, Down: phase == 1, X: px, Y: py})
	}
	if phase == 0 || phase == 3 {
		delete(u.touches, key)
	} else {
		u.touches[key] = held
	}
}

func nativeMeta(mod int) int {
	state := 0
	if mod&int(C.SDL_KMOD_SHIFT) != 0 {
		state |= 1
	}
	if mod&int(C.SDL_KMOD_ALT) != 0 {
		state |= 2
	}
	if mod&int(C.SDL_KMOD_CTRL) != 0 {
		state |= 4096
	}
	if mod&int(C.SDL_KMOD_GUI) != 0 {
		state |= 65536
	}
	if mod&int(C.SDL_KMOD_CAPS) != 0 {
		state |= 1048576
	}
	if mod&int(C.SDL_KMOD_NUM) != 0 {
		state |= 2097152
	}
	if mod&int(C.SDL_KMOD_SCROLL) != 0 {
		state |= 4194304
	}
	return state
}
func (u *nativeWorkspace) keyboard(e C.native_event) {
	scan, mod := int(e.scan), int(e.mods)
	down := e.down != 0
	if !down {
		if in, ok := nativeKeyboardInput(scan, mod, int(e.key), false, false, u.keys); ok {
			u.input(in)
		}
		return
	}
	if scan == int(C.SDL_SCANCODE_ESCAPE) {
		u.release()
		if u.fullscreen {
			if C.nui_fullscreen(u.window, 0) != 0 {
				u.fullscreen = false
			} else {
				u.errorText = C.GoString(C.SDL_GetError())
			}
		}
		u.setFocus("search")
		return
	}
	// Tab always leaves/enters the native control sequence. Android Tab remains
	// available as Ctrl+Tab while a display owns keyboard focus.
	if scan == int(C.SDL_SCANCODE_TAB) && mod&int(C.SDL_KMOD_CTRL) == 0 {
		u.traverse(mod&int(C.SDL_KMOD_SHIFT) != 0)
		return
	}
	if u.focus != "display" {
		if u.focus == "search" && scan == int(C.SDL_SCANCODE_BACKSPACE) {
			if len(u.search) > 0 {
				_, size := utf8.DecodeLastRuneInString(u.search)
				u.search = u.search[:len(u.search)-size]
				u.filterApps()
			}
			return
		}
		if u.focus == "search" && scan == int(C.SDL_SCANCODE_A) && mod&int(C.SDL_KMOD_CTRL) != 0 {
			u.search = ""
			u.filterApps()
			return
		}
		if scan == int(C.SDL_SCANCODE_RETURN) || scan == int(C.SDL_SCANCODE_SPACE) && u.focus != "search" {
			for _, widget := range u.widgets {
				if widget.id == u.focus {
					u.activate(widget)
					return
				}
			}
		}
		if scan == int(C.SDL_SCANCODE_DOWN) {
			u.traverse(false)
		} else if scan == int(C.SDL_SCANCODE_UP) {
			u.traverse(true)
		}
		if scan == int(C.SDL_SCANCODE_PAGEDOWN) {
			u.scroll += u.list.h
		} else if scan == int(C.SDL_SCANCODE_PAGEUP) {
			u.scroll -= u.list.h
		}
		return
	}
	if u.ready() == nil {
		return
	}
	// SDL text commits cover layout/IME/AltGr. Never duplicate printable keys.
	shortcut := mod&int(C.SDL_KMOD_CTRL|C.SDL_KMOD_GUI|C.SDL_KMOD_ALT) != 0 && mod&int(C.SDL_KMOD_MODE) == 0
	if shortcut && scan == int(C.SDL_SCANCODE_V) && mod&int(C.SDL_KMOD_ALT) == 0 || scan == int(C.SDL_SCANCODE_INSERT) && mod&int(C.SDL_KMOD_SHIFT) != 0 {
		text := C.SDL_GetClipboardText()
		if text != nil {
			value := C.GoString(text)
			C.SDL_free(unsafe.Pointer(text))
			if len(value) <= 65536 {
				u.input(viewerInput{Type: "text", Text: value})
			} else {
				u.errorText = "Clipboard text exceeds the phone's 64 KiB limit"
			}
		}
		return
	}
	if mod&int(C.SDL_KMOD_MODE) != 0 {
		// AltGr belongs to the local text/IME path, not remote Ctrl+Alt.
		for held, key := range u.keys {
			if key == 57 || key == 58 || key == 113 || key == 114 {
				u.input(viewerInput{Type: "key", Key: key})
				delete(u.keys, held)
			}
		}
		return
	}
	if in, ok := nativeKeyboardInput(scan, mod, int(e.key), true, u.composition != "", u.keys); ok {
		u.input(in)
	}
}

// Printable input is committed by SDL's text/IME events. Only non-printable
// keys and shortcuts get a raw down/up pair, with the down's key saved so a
// modifier or keyboard-layout change cannot strand a different Android key.
func nativeKeyboardInput(scan, mod, symbol int, down, composing bool, held map[int]int) (viewerInput, bool) {
	if !down {
		key, ok := held[scan]
		if !ok {
			return viewerInput{}, false
		}
		delete(held, scan)
		return viewerInput{Type: "key", Key: key, MetaState: nativeMeta(mod)}, true
	}
	shortcut := mod&int(C.SDL_KMOD_CTRL|C.SDL_KMOD_GUI|C.SDL_KMOD_ALT) != 0
	keypadNavigation := mod&int(C.SDL_KMOD_NUM) == 0 && scan >= int(C.SDL_SCANCODE_KP_1) && scan <= int(C.SDL_SCANCODE_KP_PERIOD)
	if composing || mod&int(C.SDL_KMOD_MODE) != 0 || (!shortcut && nativePrintable(scan) && !keypadNavigation) {
		return viewerInput{}, false
	}
	key := nativeAndroidKey(scan)
	if keypadNavigation {
		switch C.SDL_Keycode(symbol) {
		case C.SDLK_UP:
			key = 19
		case C.SDLK_DOWN:
			key = 20
		case C.SDLK_LEFT:
			key = 21
		case C.SDLK_RIGHT:
			key = 22
		case C.SDLK_HOME:
			key = 122
		case C.SDLK_END:
			key = 123
		case C.SDLK_PAGEUP:
			key = 92
		case C.SDLK_PAGEDOWN:
			key = 93
		case C.SDLK_INSERT:
			key = 124
		case C.SDLK_DELETE:
			key = 112
		}
	}
	if key == 0 {
		return viewerInput{}, false
	}
	if shortcut && symbol >= 'a' && symbol <= 'z' {
		key = 29 + symbol - 'a'
	}
	held[scan] = key
	return viewerInput{Type: "key", Key: key, Down: true, MetaState: nativeMeta(mod)}, true
}
func nativePrintable(scan int) bool {
	return scan >= int(C.SDL_SCANCODE_A) && scan <= int(C.SDL_SCANCODE_0) ||
		scan == int(C.SDL_SCANCODE_SPACE) || scan >= int(C.SDL_SCANCODE_MINUS) && scan <= int(C.SDL_SCANCODE_SLASH) && scan != int(C.SDL_SCANCODE_CAPSLOCK) ||
		scan >= int(C.SDL_SCANCODE_KP_DIVIDE) && scan <= int(C.SDL_SCANCODE_KP_PERIOD) && scan != int(C.SDL_SCANCODE_KP_ENTER) ||
		scan == int(C.SDL_SCANCODE_KP_EQUALS) || scan == int(C.SDL_SCANCODE_KP_COMMA)
}
func nativeAndroidKey(scan int) int {
	if scan >= int(C.SDL_SCANCODE_A) && scan <= int(C.SDL_SCANCODE_Z) {
		return 29 + scan - int(C.SDL_SCANCODE_A)
	}
	if scan >= int(C.SDL_SCANCODE_1) && scan <= int(C.SDL_SCANCODE_9) {
		return 8 + scan - int(C.SDL_SCANCODE_1)
	}
	if scan >= int(C.SDL_SCANCODE_F1) && scan <= int(C.SDL_SCANCODE_F12) {
		return 131 + scan - int(C.SDL_SCANCODE_F1)
	}
	if scan >= int(C.SDL_SCANCODE_KP_1) && scan <= int(C.SDL_SCANCODE_KP_9) {
		return 145 + scan - int(C.SDL_SCANCODE_KP_1)
	}
	switch C.SDL_Scancode(scan) {
	case C.SDL_SCANCODE_0:
		return 7
	case C.SDL_SCANCODE_KP_0:
		return 144
	case C.SDL_SCANCODE_BACKSPACE:
		return 67
	case C.SDL_SCANCODE_TAB:
		return 61
	case C.SDL_SCANCODE_RETURN:
		return 66
	case C.SDL_SCANCODE_KP_ENTER:
		return 160
	case C.SDL_SCANCODE_SPACE:
		return 62
	case C.SDL_SCANCODE_ESCAPE:
		return 4
	case C.SDL_SCANCODE_UP:
		return 19
	case C.SDL_SCANCODE_DOWN:
		return 20
	case C.SDL_SCANCODE_LEFT:
		return 21
	case C.SDL_SCANCODE_RIGHT:
		return 22
	case C.SDL_SCANCODE_HOME:
		return 122
	case C.SDL_SCANCODE_END:
		return 123
	case C.SDL_SCANCODE_PAGEUP:
		return 92
	case C.SDL_SCANCODE_PAGEDOWN:
		return 93
	case C.SDL_SCANCODE_INSERT:
		return 124
	case C.SDL_SCANCODE_DELETE:
		return 112
	case C.SDL_SCANCODE_LSHIFT:
		return 59
	case C.SDL_SCANCODE_RSHIFT:
		return 60
	case C.SDL_SCANCODE_LALT:
		return 57
	case C.SDL_SCANCODE_RALT:
		return 58
	case C.SDL_SCANCODE_LCTRL:
		return 113
	case C.SDL_SCANCODE_RCTRL:
		return 114
	case C.SDL_SCANCODE_LGUI:
		return 117
	case C.SDL_SCANCODE_RGUI:
		return 118
	case C.SDL_SCANCODE_CAPSLOCK:
		return 115
	case C.SDL_SCANCODE_NUMLOCKCLEAR:
		return 143
	case C.SDL_SCANCODE_SCROLLLOCK:
		return 116
	case C.SDL_SCANCODE_APPLICATION:
		return 82
	case C.SDL_SCANCODE_MINUS:
		return 69
	case C.SDL_SCANCODE_EQUALS:
		return 70
	case C.SDL_SCANCODE_LEFTBRACKET:
		return 71
	case C.SDL_SCANCODE_RIGHTBRACKET:
		return 72
	case C.SDL_SCANCODE_BACKSLASH:
		return 73
	case C.SDL_SCANCODE_SEMICOLON:
		return 74
	case C.SDL_SCANCODE_APOSTROPHE:
		return 75
	case C.SDL_SCANCODE_GRAVE:
		return 68
	case C.SDL_SCANCODE_COMMA:
		return 55
	case C.SDL_SCANCODE_PERIOD:
		return 56
	case C.SDL_SCANCODE_SLASH:
		return 76
	case C.SDL_SCANCODE_KP_DIVIDE:
		return 154
	case C.SDL_SCANCODE_KP_MULTIPLY:
		return 155
	case C.SDL_SCANCODE_KP_MINUS:
		return 156
	case C.SDL_SCANCODE_KP_PLUS:
		return 157
	case C.SDL_SCANCODE_KP_PERIOD:
		return 158
	case C.SDL_SCANCODE_KP_COMMA:
		return 159
	case C.SDL_SCANCODE_KP_EQUALS:
		return 161
	case C.SDL_SCANCODE_VOLUMEUP:
		return 24
	case C.SDL_SCANCODE_VOLUMEDOWN:
		return 25
	case C.SDL_SCANCODE_MUTE:
		return 164
	case C.SDL_SCANCODE_MEDIA_PLAY_PAUSE:
		return 85
	case C.SDL_SCANCODE_MEDIA_STOP:
		return 86
	case C.SDL_SCANCODE_MEDIA_NEXT_TRACK:
		return 87
	case C.SDL_SCANCODE_MEDIA_PREVIOUS_TRACK:
		return 88
	case C.SDL_SCANCODE_PAUSE:
		return 121
	case C.SDL_SCANCODE_PRINTSCREEN:
		return 120
	}
	return 0
}
