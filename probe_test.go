package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// topLevelKeys returns the keys of the first JSON object in s, in wire order.
func topLevelKeys(t *testing.T, s string) []string {
	t.Helper()
	typ := func(isObj, wantKey bool) struct {
		isObj   bool
		wantKey bool
	} {
		return struct {
			isObj   bool
			wantKey bool
		}{isObj, wantKey}
	}
	stack := []struct {
		isObj   bool
		wantKey bool
	}{}
	dec := json.NewDecoder(strings.NewReader(s))
	var keys []string
	started := false
	for {
		if started && len(stack) == 0 {
			break
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		started = true
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' {
				stack = append(stack, typ(true, true))
			} else if v == '[' {
				stack = append(stack, typ(false, false))
			} else {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].isObj {
					stack[len(stack)-1].wantKey = true
				}
			}
		case string:
			top := &stack[len(stack)-1]
			if top.isObj && top.wantKey {
				if len(stack) == 1 {
					keys = append(keys, v)
				}
				top.wantKey = false
			} else if top.isObj {
				top.wantKey = true
			}
		default:
			top := &stack[len(stack)-1]
			if top.isObj {
				top.wantKey = true
			}
		}
	}
	return keys
}

func TestBuildPCStatusShape(t *testing.T) {
	previous := ourInfo
	t.Cleanup(func() { ourInfo = previous })
	ourInfo = replyInfo{DeviceID: "PCID==", Name: "reaver", SKU: "HP EliteBook 840 G3"}
	b, err := buildPCStatus()
	if err != nil {
		t.Fatalf("buildPCStatus: %v", err)
	}
	var env struct {
		DeviceID     string          `json:"deviceId"`
		SrcServiceID int             `json:"srcServiceId"`
		ServiceID    int             `json:"serviceId"`
		MsgID        int64           `json:"msgId"`
		SenderID     int64           `json:"senderId"`
		Data         json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ServiceID != svcDeviceStatus || env.SrcServiceID != svcDeviceStatus {
		t.Errorf("serviceId=%d srcServiceId=%d, want %d/%d", env.ServiceID, env.SrcServiceID, svcDeviceStatus, svcDeviceStatus)
	}
	if env.SenderID != 0 {
		t.Errorf("status senderId=%d routes away from mobile-desktop capability/battery handling; want 0", env.SenderID)
	}
	if env.DeviceID != "PCID==" {
		t.Errorf("deviceId=%q, want the PC's own id", env.DeviceID)
	}
	if env.MsgID == 0 {
		t.Errorf("msgId omitted/zero; the PC always emits a non-zero counter")
	}
	var st struct {
		Cmd        int    `json:"cmd"`
		Ver        string `json:"ver"`
		DeviceName string `json:"deviceName"`
		DeviceType int    `json:"deviceType"`
		Battery    int    `json:"battery"`
		Wifi       bool   `json:"wifi"`
	}
	if err := json.Unmarshal(env.Data, &st); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if st.Cmd != 1 || st.Ver != "1" {
		t.Errorf("cmd/ver = %d/%q, want 1/\"1\"", st.Cmd, st.Ver)
	}
	if st.DeviceType != 4 {
		t.Errorf("deviceType=%d, want 4 (PC)", st.DeviceType)
	}
	if st.DeviceName != "reaver" {
		t.Errorf("deviceName=%q, want reaver", st.DeviceName)
	}
}

// TestPCStatusKeyOrder pins the field order to the phone's own full-status frame
// (capture/c8/008), which the PC must share. A reordered struct would still parse
// but would no longer be byte-comparable to the observed frame.
func TestPCStatusKeyOrder(t *testing.T) {
	got := topLevelKeys(t, mustData(t, mustBuild(t, buildPCStatus)))
	want := []string{
		"AiCapabilities", "accessAllFiles", "accountId", "adb", "battery",
		"bluetooth", "cap", "chargePlug", "data", "dataSignal", "deviceName",
		"deviceType", "foldState", "foldType", "isCharging", "lite", "model",
		"realDeviceName", "screen", "usbReady", "wallpaperState", "wifi",
		"wifiSignal", "cmd", "ver",
	}
	if len(got) != len(want) {
		t.Fatalf("data key count = %d, want %d\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key[%d]=%q, want %q\ngot  %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

// TestPCStatusMatchesObservedFrameKeys cross-checks against the captured frame so
// the expectation above cannot silently drift from the evidence.
func TestPCStatusMatchesObservedFrameKeys(t *testing.T) {
	raw := readCapture(t, "capture/c8/008_op1_609B.txt")
	var outer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		t.Fatalf("capture unmarshal: %v", err)
	}
	observed := topLevelKeys(t, string(outer.Data))
	built := topLevelKeys(t, mustData(t, mustBuild(t, buildPCStatus)))
	if len(observed) != len(built) {
		t.Fatalf("observed %d keys, built %d\nobserved %v\nbuilt    %v", len(observed), len(built), observed, built)
	}
	for i := range observed {
		if observed[i] != built[i] {
			t.Errorf("key[%d]: observed %q, built %q", i, observed[i], built[i])
		}
	}
}

func TestBuildFileStationReply(t *testing.T) {
	ourInfo = replyInfo{DeviceID: "PCID==", FP: "BASE64FP"}
	probeFSReply = "auto"
	b, err := buildFileStationReply(1001, "PHONEREAL==")
	if err != nil {
		t.Fatalf("buildFileStationReply: %v", err)
	}
	var env struct {
		SrcServiceID int `json:"srcServiceId"`
		ServiceID    int `json:"serviceId"`
		Data         struct {
			Cmd  int    `json:"cmd"`
			Data string `json:"data"`
			Ver  string `json:"ver"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ServiceID != probeFSSvc || env.SrcServiceID != probeFSSvc {
		t.Errorf("serviceId=%d srcServiceId=%d, want %d", env.ServiceID, env.SrcServiceID, probeFSSvc)
	}
	if env.Data.Cmd != 1001 || env.Data.Ver != "1.0" {
		t.Errorf("cmd/ver = %d/%q, want 1001/\"1.0\"", env.Data.Cmd, env.Data.Ver)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(env.Data.Data), &inner); err != nil {
		t.Fatalf("inner data is not a JSON string: %v (%q)", err, env.Data.Data)
	}
	if inner["deviceId"] != "PHONEREAL==" {
		t.Errorf("inner deviceId=%v, want the phone's realDeviceId", inner["deviceId"])
	}
	if _, ok := inner["nfsport"]; !ok {
		t.Errorf("inner payload lacks nfsport: %v", inner)
	}
}

func TestBuildEchoRewritesEnvelope(t *testing.T) {
	ourInfo = replyInfo{DeviceID: "PCID=="}
	in := []byte(`{"data":{"cmd":1,"data":{"version":1},"msgId":1},"deviceId":"PHONE==","msgId":999,"senderId":1,"serviceId":9,"srcServiceId":9}`)
	out, err := buildEcho(svcClipboard, in)
	if err != nil {
		t.Fatalf("buildEcho: %v", err)
	}
	var env struct {
		DeviceID string         `json:"deviceId"`
		Service  int            `json:"serviceId"`
		MsgID    int64          `json:"msgId"`
		SenderID int64          `json:"senderId"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.DeviceID != "PCID==" {
		t.Errorf("deviceId=%q, want our id", env.DeviceID)
	}
	if env.MsgID == 999 {
		t.Errorf("msgId not rewritten")
	}
	if env.Service != svcClipboard {
		t.Errorf("serviceId=%d, want %d", env.Service, svcClipboard)
	}
	if _, ok := env.Data["data"]; !ok {
		t.Errorf("inner data not preserved: %v", env.Data)
	}
}

func TestEnvelopeOf(t *testing.T) {
	sid, cmd, ok := envelopeOf([]byte(`{"serviceId":25,"data":{"cmd":20,"ver":"1"}}`))
	if !ok || sid != 25 || cmd == nil || *cmd != 20 {
		t.Fatalf("got sid=%d cmd=%v ok=%v", sid, cmd, ok)
	}
	if _, _, ok := envelopeOf([]byte(`not json`)); ok {
		t.Fatalf("non-JSON accepted")
	}
}

func TestParseSvcSet(t *testing.T) {
	got := parseSvcSet("25, 9,,16")
	for _, n := range []int{25, 9, 16} {
		if !got[n] {
			t.Errorf("missing %d in %v", n, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %v, want 3 entries", got)
	}
}

// --- helpers ---

func mustBuild(t *testing.T, f func() ([]byte, error)) []byte {
	t.Helper()
	b, err := f()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return b
}

func mustData(t *testing.T, frame []byte) string {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return string(env.Data)
}

func readCapture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("capture %s unavailable: %v", path, err)
	}
	return string(b)
}
