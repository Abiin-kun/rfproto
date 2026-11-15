package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBuildDiscoveryReply(t *testing.T) {
	b, err := buildDiscoveryReply(replyInfo{
		DeviceID: "KIfv22hgTCCANmbV3jiOTg==",
		Name:     "TESTPC",
		FP:       "x2fngiUKwgjrEtMjrPgQpYYeKdZhADbSLi46lL6OH9U=",
		IP:       "192.168.1.103",
		Port:     19527,
		Token:    "OK",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatalf("not valid json: %s", b)
	}
	var pretty any
	json.Unmarshal(b, &pretty)
	out, _ := json.MarshalIndent(pretty, "", "  ")
	t.Logf("\n%s", out)
}

// TestHandleAutoReply drives the real handler with the captured phone hello and
// checks that the recovered server reply is produced.
func TestHandleAutoReply(t *testing.T) {
	setupPairingTest(t)
	prevMode, prevInfo, prevCapture := replyToFirst, ourInfo, captureDir
	t.Cleanup(func() { replyToFirst, ourInfo, captureDir = prevMode, prevInfo, prevCapture })
	replyToFirst = "auto"
	captureDir = ""
	ourInfo = replyInfo{DeviceID: "KIfv22hgTCCANmbV3jiOTg==", Name: "TESTPC",
		FP: "x2fngiUKwgjrEtMjrPgQpYYeKdZhADbSLi46lL6OH9U=", IP: "192.168.1.103", Port: 19527, Token: "OK"}

	c := pairDial(t, pairingServer(t))

	hello := `{"data":{"apkVerCode":9030130,"cmd":0,"device":{"deviceId":"Qeq6rLWsQMqgWSJA5+vNHg=="},"token":"Z7c7VuHJDq61vzYtKfc3D9FhQ6dOeeyNfJ3pexcGnIY=","ver":3},"deviceId":"Qeq6rLWsQMqgWSJA5+vNHg==","msgId":76,"senderId":1,"serviceId":0,"srcServiceId":0}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, reply, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	var got struct {
		DeviceID string `json:"deviceId"`
		Data     struct {
			Ver   int    `json:"ver"`
			Token string `json:"token"`
			Dev   struct {
				DeviceID string `json:"deviceId"`
				Name     string `json:"deviceName"`
			} `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("reply not json: %v: %s", err, reply)
	}
	if got.Data.Ver != 3 || got.Data.Token != "OK" || got.DeviceID == "" || got.Data.Dev.DeviceID == "" {
		t.Fatalf("unexpected reply: %s", reply)
	}
	t.Logf("reply OK: %s", reply)
}

// TestOurDeviceMatchesRecoveredValues pins the device.Device identity fields to
// the values the real PC installs. Evidence:
//
//	cap              movw $0x28ba,0x18141c4f4   (device.init.0)
//	caps             map @ 0x1813cf088          (device.init.0, via CurrentDevice +0x60)
//	serviceVersions  map @ 0x1813cf080          (device.init.0, via CurrentDevice +0x80)
//	verCode          0x1813505f8 = 900001000
//	verName          0x1813a0400 = "09.0.0.001.000"
//	minVerCode       map @ 0x1813cefd8          (common/constants.init)
func TestOurDeviceMatchesRecoveredValues(t *testing.T) {
	if pcCapability != 0x28ba {
		t.Fatalf("pcCapability = %#x, want 0x28ba", pcCapability)
	}
	if pcVerCode != 900001000 || pcVerName != "09.0.0.001.000" {
		t.Fatalf("PC identity = %d/%q", pcVerCode, pcVerName)
	}

	dev := ourDevice(replyInfo{DeviceID: "A", Name: "N"})
	if dev.Capability != 0x28ba {
		t.Fatalf("reply cap = %d, want 10426", dev.Capability)
	}
	// caps must serialise as a JSON object (map), not an array: the field is a
	// map[int]int in device.Device (+0x60), the same reflect kind as
	// serviceVersions and minVerCode.
	b, err := json.Marshal(dev)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["caps"]); !strings.HasPrefix(got, "{") {
		t.Fatalf("caps must be an object, got %s", got)
	}
	for _, k := range []string{"caps", "serviceVersions", "minVerCode"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("%s missing from %s", k, b)
		}
	}
	if len(pcServiceVersions) != 20 {
		t.Fatalf("pcServiceVersions has %d entries, want 20", len(pcServiceVersions))
	}
	if len(pcCapabilities) != 13 {
		t.Fatalf("pcCapabilities has %d entries, want 13", len(pcCapabilities))
	}
	// The BLE advert carries the low byte of cap: 0x28ba -> 0xba.
	if pl, err := bleAdvPayload("06T3cwaNbuA4xDLwylLvEw==", byte(pcCapability&0xff)); err == nil {
		if pl[1] != 0xba {
			t.Fatalf("advert cap byte = %#x, want 0xba", pl[1])
		}
	}
}

// TestReplyCarriesSnAndSku pins the two device.Device fields the phone rejects
// us without. Live logcat (15:14:49.624) showed:
//
//	[ DBManager ] - insert RemoteDevice error!
//	SQLiteConstraintException: NOT NULL constraint failed: remote_device.sn
//
// so the reply must carry a non-empty `sn`, and `Sku` per winapi.dll's tag.
func TestReplyCarriesSnAndSku(t *testing.T) {
	prev := ourInfo
	prevMode := replyToFirst
	defer func() { ourInfo, replyToFirst = prev, prevMode }()

	ourInfo = replyInfo{
		DeviceID: "KIfv22hgTCCANmbV3jiOTg==", Name: "TESTPC", FP: "FP", IP: "10.0.0.1", Port: 1,
		SN: "4f2a1c8e-9b3d-4a6f-8e21-0c5d7b9a1e33", SKU: "90NX00ABUS",
	}
	replyToFirst = "auto"

	b, err := buildDiscoveryReply(ourInfo)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Data struct {
			Device map[string]json.RawMessage `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"sn":  `"4f2a1c8e-9b3d-4a6f-8e21-0c5d7b9a1e33"`,
		"Sku": `"90NX00ABUS"`,
	} {
		got, ok := raw.Data.Device[k]
		if !ok {
			t.Fatalf("%s missing from device: %s", k, b)
		}
		if string(got) != want {
			t.Fatalf("device.%s = %s, want %s", k, got, want)
		}
	}
	if _, ok := raw.Data.Device["sku"]; ok {
		t.Fatalf("default skukey must be the recovered tag Sku, got sku in %s", b)
	}

	// -skukey sku / both are the A/B escape hatch.
	for _, tc := range []struct {
		mode                  string
		wantSku, wantSkuPhone bool
	}{
		{"sku", false, true},
		{"both", true, true},
		{"Sku", true, false},
		{"", true, false},
	} {
		ri := ourInfo
		ri.SKUKey = tc.mode
		dev := ourDevice(ri)
		if (dev.SKU != "") != tc.wantSku || (dev.SKUPhone != "") != tc.wantSkuPhone {
			t.Fatalf("skukey %q -> SKU=%q SKUPhone=%q", tc.mode, dev.SKU, dev.SKUPhone)
		}
	}
}

func TestIdentityDefaults(t *testing.T) {
	if g := guidFromHex("4f2a1c8e9b3d4a6f8e210c5d7b9a1e33"); g != "4f2a1c8e-9b3d-4a6f-8e21-0c5d7b9a1e33" {
		t.Fatalf("guidFromHex = %q", g)
	}
	if g := guidFromHex("not-hex"); g != "" {
		t.Fatalf("guidFromHex(bad) = %q, want empty", g)
	}
	// The host must always yield a non-empty machine identity.
	if g := machineGUID(t.TempDir()); g == "" {
		t.Fatal("machineGUID returned empty")
	}
	if p := machineProductName(); p == "" {
		t.Fatal("machineProductName returned empty")
	}
}

// capturedHello / capturedPairConfirm are verbatim frames the phone sent during
// the live session recorded in capture/001_op1_1124B.txt and 002_op1_530B.txt.
const capturedHello = `{"data":{"apkVerCode":9030130,"cmd":0,"device":{"deviceId":"Qeq6rLWsQMqgWSJA5+vNHg=="},"token":"Z7c7VuHJDq61vzYtKfc3D9FhQ6dOeeyNfJ3pexcGnIY=","ver":3},"deviceId":"Qeq6rLWsQMqgWSJA5+vNHg==","msgId":76,"senderId":1,"serviceId":0,"srcServiceId":0}`

const capturedPairConfirm = `{"data":{"apkVerCode":9030130,"apkVerName":"09.0.3.013.0","cmd":1,"connType":0,"device":{"cap":5365,"cloudPush":0,"deviceId":"Icca7GgDY7+U16HiYxvNrg==","deviceName":"moto g34 5G","i3":1,"key":"9bUYkJRZy7lOoQ1LQ64PEBfrZwiKT6dd6uyvdn8gnzw=","os":1,"realDeviceId":"Icca7GgDY7+U16HiYxvNrg==","scType":0,"sku":"XT2363-5","sn":"ZA222L3QZQ","type":1,"verCode":0},"limitService":0,"paired":1,"sdk":35,"ver":3},"deviceId":"06T3cwaNbuA4xDLwylLvEw==","msgId":79,"senderId":1,"serviceId":0,"srcServiceId":0}`

func TestPairKeyFrom(t *testing.T) {
	key, ok := pairKeyFrom([]byte(capturedPairConfirm))
	if !ok {
		t.Fatal("pair-confirm not recognised")
	}
	if key != "9bUYkJRZy7lOoQ1LQ64PEBfrZwiKT6dd6uyvdn8gnzw=" {
		t.Fatalf("key = %q", key)
	}
	// The hello (cmd 0) and a DeviceStatus message (serviceId 25, cmd 1) are not
	// pair-confirms.
	if _, ok := pairKeyFrom([]byte(capturedHello)); ok {
		t.Fatal("hello misread as pair-confirm")
	}
	status := `{"data":{"cmd":1},"deviceId":"x","msgId":84,"senderId":1,"serviceId":25,"srcServiceId":25}`
	if _, ok := pairKeyFrom([]byte(status)); ok {
		t.Fatal("DeviceStatus misread as pair-confirm")
	}
}

func TestBuildPairAck(t *testing.T) {
	b, err := buildPairAck(replyInfo{DeviceID: "KIfv22hgTCCANmbV3jiOTg==", Name: "TESTPC",
		FP: "x2f", IP: "192.168.1.103", Port: 19527}, "SHAREDKEY=", 1)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		DeviceID     string `json:"deviceId"`
		SrcServiceID int    `json:"srcServiceId"`
		ServiceID    int    `json:"serviceId"`
		Data         struct {
			Ver    int `json:"ver"`
			Cmd    int `json:"cmd"`
			Device struct {
				DeviceID string `json:"deviceId"`
				Key      string `json:"key"`
			} `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.Ver != 3 || got.Data.Cmd != 1 || got.Data.Device.Key != "SHAREDKEY=" {
		t.Fatalf("unexpected pair-ack: %s", b)
	}
	if got.ServiceID != 0 || got.SrcServiceID != 0 || got.DeviceID == "" {
		t.Fatalf("unexpected envelope: %s", b)
	}
	t.Logf("pair-ack: %s", b)
}

// TestHandlePairAck runs the full observed sequence against the real handler:
// hello -> discovery reply, pair-confirm -> pair-ack.
func TestHandlePairAck(t *testing.T) {
	setupPairingTest(t)
	prevMode, prevAck, prevCmd, prevInfo, prevCapture := replyToFirst, pairAckMode, pairAckCmd, ourInfo, captureDir
	t.Cleanup(func() {
		replyToFirst, pairAckMode, pairAckCmd, ourInfo, captureDir = prevMode, prevAck, prevCmd, prevInfo, prevCapture
	})
	replyToFirst, pairAckMode, pairAckCmd, captureDir = "auto", "auto", 1, ""
	ourInfo = replyInfo{DeviceID: "KIfv22hgTCCANmbV3jiOTg==", Name: "TESTPC",
		FP: "x2f", IP: "192.168.1.103", Port: 19527, Token: "OK"}

	c := pairDial(t, pairingServer(t))

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte(capturedHello)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("no discovery reply: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(capturedPairConfirm)); err != nil {
		t.Fatal(err)
	}
	_, ack, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("no pair-ack: %v", err)
	}
	var got struct {
		Data struct {
			Cmd    int `json:"cmd"`
			Device struct {
				Key string `json:"key"`
			} `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(ack, &got); err != nil {
		t.Fatalf("pair-ack not json: %v: %s", err, ack)
	}
	if got.Data.Cmd != 1 || got.Data.Device.Key != "9bUYkJRZy7lOoQ1LQ64PEBfrZwiKT6dd6uyvdn8gnzw=" {
		t.Fatalf("unexpected pair-ack: %s", ack)
	}
	t.Logf("pair-ack OK: %s", ack)
}
