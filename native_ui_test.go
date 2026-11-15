package main

import "testing"

func TestStreamShapeUsesPhoneRotation(t *testing.T) {
	cases := []struct {
		name                string
		session             viewerSession
		frameW, frameH      int
		wantW, wantH, wantA int
	}{
		{"portrait raster", viewerSession{Width: 360, Height: 800}, 0, 0, 360, 800, 0},
		{"landscape raster", viewerSession{Width: 1280, Height: 720}, 0, 0, 1280, 720, 0},
		{"phone quarter turn is counterclockwise", viewerSession{Width: 360, Height: 800, Rotation: 1}, 360, 800, 800, 360, 270},
		{"phone half turn", viewerSession{Width: 360, Height: 800, Rotation: 2}, 360, 800, 360, 800, 180},
		{"phone reverse quarter turn", viewerSession{Width: 360, Height: 800, Rotation: 3}, 360, 800, 800, 360, 90},
		{"auto follows landscape buffer rotation", viewerSession{Width: 1280, Height: 720, Rotation: 1}, 1280, 720, 720, 1280, 270},
		{"pending remote resize does not rotate old pixels", viewerSession{Orientation: "auto", OrientationPending: "landscape"}, 360, 800, 360, 800, 0},
		{"confirmed remote resize does not add local rotation", viewerSession{Orientation: "landscape"}, 800, 360, 800, 360, 0},
		{"decoded dimensions take precedence", viewerSession{Width: 360, Height: 800}, 800, 360, 800, 360, 0},
		{"fallback size", viewerSession{}, 0, 0, 1280, 720, 0},
	}
	for _, tc := range cases {
		w, h, angle := streamShape(tc.session, tc.frameW, tc.frameH)
		if w != tc.wantW || h != tc.wantH || angle != tc.wantA {
			t.Errorf("%s: got (%d,%d,%d), want (%d,%d,%d)", tc.name, w, h, angle, tc.wantW, tc.wantH, tc.wantA)
		}
	}
}

func TestRotatePointInverseMapping(t *testing.T) {
	cases := []struct {
		angle        int
		u, v         float64
		wantU, wantV float64
	}{
		{0, 0.25, 0.10, 0.25, 0.10},
		{90, 0.25, 0.10, 0.10, 0.75},
		{180, 0.25, 0.10, 0.75, 0.90},
		{270, 0.25, 0.10, 0.90, 0.25},
	}
	for _, tc := range cases {
		u, v := rotatePoint(tc.u, tc.v, tc.angle)
		if u != tc.wantU || v != tc.wantV {
			t.Errorf("angle %d: got (%.2f,%.2f), want (%.2f,%.2f)", tc.angle, u, v, tc.wantU, tc.wantV)
		}
	}
}

func TestNativeKeyboardTextAndShortcutSymmetry(t *testing.T) {
	// SDL scancodes use USB HID positions; modifier 0x40 is left Control.
	held := make(map[int]int)
	for _, tc := range []struct{ scan, mod, symbol int }{{4, 0, 'a'}, {4, 1, 'A'}, {44, 0, ' '}, {4, 0x4000, 'ä'}} {
		if in, ok := nativeKeyboardInput(tc.scan, tc.mod, tc.symbol, true, false, held); ok {
			t.Fatalf("printable input generated duplicate raw key: %+v", in)
		}
		if _, ok := nativeKeyboardInput(tc.scan, 0, tc.symbol, false, false, held); ok {
			t.Fatal("text-only key generated unmatched key-up")
		}
	}
	in, ok := nativeKeyboardInput(4, 0x40, 'a', true, false, held)
	if !ok || in.Type != "key" || in.Key != 29 || !in.Down || in.MetaState != 4096 {
		t.Fatalf("Ctrl+A down: %+v, %v", in, ok)
	}
	// A changed layout/modifier at release must release the original Android key.
	in, ok = nativeKeyboardInput(4, 0, 'z', false, false, held)
	if !ok || in.Key != 29 || in.Down || in.MetaState != 0 {
		t.Fatalf("Ctrl+A up: %+v, %v", in, ok)
	}
	if _, ok = nativeKeyboardInput(4, 0, 'a', false, false, held); ok {
		t.Fatal("duplicate key-up was forwarded")
	}
	if _, ok = nativeKeyboardInput(40, 0, 0, true, true, held); ok {
		t.Fatal("IME composition generated a raw Enter")
	}
	if len(held) != 0 {
		t.Fatalf("keys remained held: %v", held)
	}
}

func TestLauncherFilterAndSelection(t *testing.T) {
	front := &frontend{state: viewerState{Apps: []viewerApp{
		{ID: "clock", Name: "Clock", Package: "com.example.clock"},
		{ID: "maps", Name: "Maps", Package: "com.example.maps"},
		{ID: "photos", Name: "Photos", Package: "com.example.photos"},
	}}}
	l := nativeLauncher{front: front, selected: 1}
	l.filter()
	if len(l.apps) != 3 || l.selected != 0 {
		t.Fatalf("initial filter: apps=%d selected=%d", len(l.apps), l.selected)
	}
	l.search = "map"
	l.filter()
	if len(l.apps) != 1 || l.apps[0].ID != "maps" || l.selected != 0 {
		t.Fatalf("search filter: %+v selected=%d", l.apps, l.selected)
	}
	l.search = "nothing"
	l.filter()
	if len(l.apps) != 0 || l.selected != -1 {
		t.Fatalf("empty filter: apps=%d selected=%d", len(l.apps), l.selected)
	}
}

func TestMonogramAndTintAreStable(t *testing.T) {
	if got := monogram("Google Maps"); got != "GM" {
		t.Fatalf("monogram: %q", got)
	}
	if got := monogram("  "); got == "" {
		t.Fatal("empty monogram")
	}
	if appTint("com.example.clock") != appTint("com.example.clock") {
		t.Fatal("identity tint is not deterministic")
	}
	if appTint("com.example.clock") == appTint("com.example.maps") {
		t.Fatal("identity tints collide for different packages")
	}
}

func TestLauncherWheelScrollSurvivesLayout(t *testing.T) {
	front := &frontend{state: viewerState{Connected: true, Apps: make([]viewerApp, 80)}}
	l := nativeLauncher{front: front}
	l.width, l.height = 1000, 600
	l.filter()
	l.layout()
	x, y := l.grid.center()
	l.wheel(x, y, -2)
	scrolled := l.scroll
	l.layout()
	if scrolled <= 0 || l.scroll != scrolled {
		t.Fatalf("wheel scroll snapped back: before=%v after=%v", scrolled, l.scroll)
	}
	l.moveSelection(1)
	if l.scroll != 0 {
		t.Fatalf("keyboard selection did not return into view: %v", l.scroll)
	}
}

func TestStreamPointerFollowsPhoneRotationAndLetterboxing(t *testing.T) {
	s := nativeStream{
		front:   &frontend{state: viewerState{Connected: true}},
		session: viewerSession{State: "connected", DisplayID: 3, Width: 360, Height: 800, Rotation: 1},
		video:   nativeRect{100, 50, 200, 100},
	}
	_, _, s.angle = streamShape(s.session, 360, 800)
	x, y, ok := s.point(150, 62.5, false)
	if !ok || x != 315 || y != 200 {
		t.Fatalf("rotated pointer = (%d,%d,%v), want (315,200,true)", x, y, ok)
	}
	if _, _, ok := s.point(99, 60, false); ok {
		t.Fatal("letterbox press reached the phone")
	}
	x, y, ok = s.point(400, 200, true)
	if !ok || x != 0 || y != 799 {
		t.Fatalf("captured edge release = (%d,%d,%v), want (0,799,true)", x, y, ok)
	}
}
