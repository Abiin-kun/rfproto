package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildScreenShareRequest pins the recovered service-202 frame.
// services/WebView.startScreenShare (0x1808aae40) formats
//
//	{"shareType":"%s","setAudio":true,"setScale":150,"setPosX":0,"setPosY":0,"deviceid":"%s"}
//
// and hands it to SendServiceMsgByMap(0xca=202, …) under the map key "data",
// next to cmd=25 and ver="1". json.Marshal sorts the map's keys, so `data` is a
// JSON string and the payload is {"cmd":25,"data":"…","ver":"1"}.
func TestBuildScreenShareRequest(t *testing.T) {
	saved := ourInfo.DeviceID
	ourInfo.DeviceID = "06T3cwaNbuA4xDLwylLvEw=="
	defer func() { ourInfo.DeviceID = saved }()

	const phone = "IXDUhJUYzqaLVA1Zl3wK5Q=="
	got, err := buildScreenShareRequest(phone, "mirror")
	if err != nil {
		t.Fatal(err)
	}

	var env struct {
		DeviceID     string          `json:"deviceId"`
		SrcServiceID int             `json:"srcServiceId"`
		ServiceID    int             `json:"serviceId"`
		MsgID        int64           `json:"msgId"`
		SenderID     int64           `json:"senderId"`
		Data         json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatal(err)
	}
	if env.SrcServiceID != svcPCScreenShare || env.ServiceID != svcPCScreenShare {
		t.Errorf("serviceId=%d srcServiceId=%d, want both 202", env.ServiceID, env.SrcServiceID)
	}
	// The envelope id is the sender's own (SendMessage defaults an empty id to
	// GetDeviceId for serviceId >= 99); the phone is only named inside the payload.
	if env.DeviceID != ourInfo.DeviceID {
		t.Errorf("envelope deviceId = %q, want ours %q", env.DeviceID, ourInfo.DeviceID)
	}
	if env.MsgID == 0 {
		t.Error("msgId omitted, but the PC always sends ++globalMsgId")
	}
	if env.SenderID != senderIDReadyForAssist {
		t.Errorf("senderId = %d, want %d", env.SenderID, senderIDReadyForAssist)
	}

	// `data` under the envelope is the {cmd,data,ver} payload object, and *its*
	// `data` is a JSON string. Pin the string form in the raw bytes, then rely on
	// the decode below to prove it (an object would not unmarshal into a string).
	if !strings.Contains(string(got), `"data":"{\"shareType\"`) {
		t.Fatalf("payload `data` is not a JSON string holding the share json: %s", got)
	}
	var payload screenSharePayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Cmd != pcScreenShareCmd {
		t.Errorf("data.cmd = %d, want %d", payload.Cmd, pcScreenShareCmd)
	}
	if payload.Ver != pcScreenShareVer {
		t.Errorf("data.ver = %q, want %q", payload.Ver, pcScreenShareVer)
	}
	want := `{"shareType":"mirror","setAudio":true,"setScale":150,"setPosX":0,"setPosY":0,` +
		`"deviceid":"` + phone + `"}`
	if payload.Data != want {
		t.Errorf("data string mismatch\n got: %s\nwant: %s", payload.Data, want)
	}

	// The extended-display variant differs only in shareType.
	ext, err := buildScreenShareRequest(phone, "extend")
	if err != nil {
		t.Fatal(err)
	}
	var env2 struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(ext, &env2); err != nil {
		t.Fatal(err)
	}
	var p2 screenSharePayload
	if err := json.Unmarshal(env2.Data, &p2); err != nil {
		t.Fatal(err)
	}
	if p2.Data == payload.Data {
		t.Error("mirror and extend produced the same inner payload")
	}
}

// TestShareTypesMatchTheBinary pins the accepted share types to the literals in
// services/WebView.startScreenShare.
func TestShareTypesMatchTheBinary(t *testing.T) {
	for _, s := range []string{"mirror", "extend"} {
		if !screenShareTypes[s] {
			t.Errorf("share type %q not accepted", s)
		}
	}
	for _, s := range []string{"MIRROR", "EXTEND", "duplicate", ""} {
		if screenShareTypes[s] {
			t.Errorf("share type %q unexpectedly accepted", s)
		}
	}
}

// TestPeerDeviceID pins how the phone's own id is learned. The envelope
// deviceId is not reliable: capture/c1/001 (hello) carries the phone's id but
// 002 (pair-confirm) carries *our* id, while data.device always describes the
// sender.
func TestPeerDeviceID(t *testing.T) {
	saved := ourInfo.DeviceID
	ourInfo.DeviceID = "06T3cwaNbuA4xDLwylLvEw=="
	defer func() { ourInfo.DeviceID = saved }()

	// The real hello (capture/c1/001, abridged): both fields are the phone.
	hello := []byte(`{"data":{"cmd":0,"device":{"deviceId":"IXDUhJUYzqaLVA1Zl3wK5Q==",` +
		`"realDeviceId":"Icca7GgDY7+U16HiYxvNrg==","deviceName":"moto g34 5G","sn":"ZA222L3QZQ"}},` +
		`"deviceId":"IXDUhJUYzqaLVA1Zl3wK5Q==","msgId":2,"senderId":1,"serviceId":0,"srcServiceId":0}`)
	if got := peerDeviceID(hello); got != "IXDUhJUYzqaLVA1Zl3wK5Q==" {
		t.Errorf("hello peerDeviceID = %q", got)
	}

	// The pair-confirm (capture/c1/002): the envelope holds OUR id, so the
	// envelope must not be used as "the phone".
	confirm := []byte(`{"deviceId":"06T3cwaNbuA4xDLwylLvEw==","msgId":3,"senderId":1,` +
		`"serviceId":0,"srcServiceId":0,"data":{"cmd":1,"token":"OK"}}`)
	if got := peerDeviceID(confirm); got != "" {
		t.Errorf("pair-confirm peerDeviceID = %q, want empty (that id is ours)", got)
	}

	if got := peerDeviceID([]byte("not json")); got != "" {
		t.Errorf("garbage peerDeviceID = %q", got)
	}
}

// TestPeerRealDeviceID pins extraction of the phone's stable realDeviceId. It is
// the id the phone uses inside its own service payloads (E24), so it is the
// candidate `data.deviceid` for -shareid real.
func TestPeerRealDeviceID(t *testing.T) {
	hello := []byte(`{"data":{"cmd":0,"device":{"deviceId":"IXDUhJUYzqaLVA1Zl3wK5Q==",` +
		`"realDeviceId":"Icca7GgDY7+U16HiYxvNrg=="}},"deviceId":"IXDUhJUYzqaLVA1Zl3wK5Q=="}`)
	if got := peerRealDeviceID(hello); got != "Icca7GgDY7+U16HiYxvNrg==" {
		t.Errorf("hello realDeviceId = %q, want Icca7GgDY7+U16HiYxvNrg==", got)
	}

	// When the two ids coincide (or only deviceId is present) there is nothing
	// extra to learn, so the selector must not fabricate a target.
	same := []byte(`{"data":{"device":{"deviceId":"same","realDeviceId":"same"}}}`)
	if got := peerRealDeviceID(same); got != "" {
		t.Errorf("identical ids = %q, want empty", got)
	}
	if got := peerRealDeviceID([]byte(`{"data":{"device":{"deviceId":"only"}}}`)); got != "" {
		t.Errorf("no realDeviceId = %q, want empty", got)
	}
	if got := peerRealDeviceID([]byte("not json")); got != "" {
		t.Errorf("garbage realDeviceId = %q", got)
	}
}

// TestShareTarget pins -shareid's choice of the id that goes in data.deviceid,
// including the fallback when the requested id was not learned.
func TestShareTarget(t *testing.T) {
	const device = "HcvQqM1wLAxMnOBzd4vpOg=="
	const real = "Icca7GgDY7+U16HiYxvNrg=="
	saved := shareIDMode
	defer func() { shareIDMode = saved }()

	shareIDMode = "device"
	if got := shareTarget(device, real); got != device {
		t.Errorf("device mode = %q, want %q", got, device)
	}
	shareIDMode = "real"
	if got := shareTarget(device, real); got != real {
		t.Errorf("real mode = %q, want %q", got, real)
	}
	// Missing the requested id falls back to whichever one exists.
	if got := shareTarget(device, ""); got != device {
		t.Errorf("real mode w/o realId = %q, want %q", got, device)
	}
	if got := shareTarget("", real); got != real {
		t.Errorf("device mode w/o deviceId = %q, want %q", got, real)
	}
	if got := shareTarget("", ""); got != "" {
		t.Errorf("no ids = %q, want empty", got)
	}
}

// TestAdvertise202Probe pins the -advertise202 probe: it must add service 202 to
// both advertised maps, and nothing else about them should change.
func TestAdvertise202Probe(t *testing.T) {
	capN, verN := len(pcCapabilities), len(pcServiceVersions)
	if _, ok := pcCapabilities[svcPCScreenShare]; ok {
		t.Skip("202 already advertised; probe is a no-op at this state")
	}
	advertise202Probe()
	defer func() {
		delete(pcCapabilities, svcPCScreenShare)
		delete(pcServiceVersions, svcPCScreenShare)
	}()
	if got := pcCapabilities[svcPCScreenShare]; got != 2 {
		t.Errorf("caps[202] = %d, want 2", got)
	}
	if got := pcServiceVersions[svcPCScreenShare]; got != 2 {
		t.Errorf("serviceVersions[202] = %d, want 2", got)
	}
	if len(pcCapabilities) != capN+1 || len(pcServiceVersions) != verN+1 {
		t.Errorf("probe changed more than 202: caps %d->%d, serviceVersions %d->%d",
			capN, len(pcCapabilities), verN, len(pcServiceVersions))
	}
}
