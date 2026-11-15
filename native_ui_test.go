package main

import "testing"

func TestNativePointerCoordinates(t *testing.T) {
	cases := []struct {
		name   string
		x, y   float32
		clamp  bool
		px, py int
		ok     bool
	}{
		{"center", 420, 230, false, 640, 360, true},
		{"top left", 100, 50, false, 0, 0, true},
		{"letterbox", 99, 230, false, 0, 0, false},
		{"bottom edge", 420, 410, false, 0, 0, false},
		{"captured drag", 0, 1000, true, 0, 719, true},
	}
	for _, density := range []float32{1, 2} {
		for _, rotation := range []int{0, 1} {
			u := nativeWorkspace{active: "v1", density: density, display: nativeRect{100, 50, 640, 360},
				state: viewerState{Connected: true, Sessions: []viewerSession{{ID: "v1", State: "connected", DisplayID: 2, Width: 1280, Height: 720, Rotation: rotation}}}}
			for _, tc := range cases {
				x, y, ok := u.point(tc.x, tc.y, tc.clamp)
				if x != tc.px || y != tc.py || ok != tc.ok {
					t.Errorf("%s density=%v rotation=%d: got (%d,%d,%v), want (%d,%d,%v)", tc.name, density, rotation, x, y, ok, tc.px, tc.py, tc.ok)
				}
			}
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

func TestNativeFocusLossReleasesLocalInput(t *testing.T) {
	// Remote held-input cleanup is separately exercised in streaming tests.
	u := nativeWorkspace{focus: "display", composition: "composing", keys: map[int]int{4: 29},
		buttons: map[int]bool{1: true}, touches: map[nativeTouchKey]nativeTouch{{device: 1, finger: 2}: {id: 0, x: 20, y: 30}}}
	u.loseFocus()
	if u.focus != "" || u.composition != "" || len(u.keys) != 0 || len(u.buttons) != 0 || len(u.touches) != 0 {
		t.Fatalf("focus loss retained input: focus=%q composition=%q keys=%v buttons=%v touches=%v", u.focus, u.composition, u.keys, u.buttons, u.touches)
	}
	if _, ok := nativeKeyboardInput(4, 0, 'a', false, false, u.keys); ok {
		t.Fatal("focus loss left an unmatched remote key-up")
	}
}

func TestNativeRemoteCloseForgetsHeldInput(t *testing.T) {
	h, _, _, _ := streamingFixture(t)
	s := h.newSessionLocked("app", catalogApp{Pkg: "example.clock"}, "Clock", 1280, 720)
	s.State, s.DisplayID, s.wireID = "connected", 3, 42
	u := nativeWorkspace{hub: h, state: h.state(), active: s.ID, focus: "display",
		keys: map[int]int{4: 29}, buttons: map[int]bool{1: true},
		touches: map[nativeTouchKey]nativeTouch{{device: 1, finger: 2}: {id: 0}}}
	h.removeSessionLocked(s, false)
	u.handleHub(map[string]any{"type": "state", "state": h.state()})
	if u.errorText != "" || u.active != "" || len(u.keys) != 0 || len(u.buttons) != 0 || len(u.touches) != 0 {
		t.Fatalf("remote close retained input or reported a stale-session error: %+v", u)
	}
}
