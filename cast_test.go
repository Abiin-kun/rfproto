package main

import (
	"encoding/json"
	"testing"
)

// The cast frame is the phone->PC mirror trigger recovered from the phone app
// (docs/INVESTIGATION.md E37): serviceId 1001, FeaturesServiceRequest.cmd 11,
// CastPhoneReqData.cmd 0/1/2/100. Pin the shape so a refactor cannot silently
// change a field name the phone's Gson parser looks up.
func TestCastRequestShape(t *testing.T) {
	oldInfo, oldSvc, oldCmd, oldW, oldH, oldFPS, oldVer, oldDev, oldListen :=
		ourInfo, castServiceID, castOuterCmd, castWidth, castHeight, castFPS, castVersion, castDeviceIDMode, castListen
	defer func() {
		ourInfo, castServiceID, castOuterCmd, castWidth, castHeight, castFPS, castVersion, castDeviceIDMode, castListen =
			oldInfo, oldSvc, oldCmd, oldW, oldH, oldFPS, oldVer, oldDev, oldListen
	}()

	ourInfo.DeviceID = "PC-DEVICE-ID=="
	castServiceID = svcFeatures
	castOuterCmd = castCmdRequest
	castWidth, castHeight, castFPS = 1920, 1080, 30
	castVersion = "1.0"
	castDeviceIDMode = "device"
	castListen = 15

	b, err := buildCastRequest("mirror", "PHONE-REAL-ID==")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var env struct {
		DeviceID     string `json:"deviceId"`
		SrcServiceID int    `json:"srcServiceId"`
		ServiceID    int    `json:"serviceId"`
		MsgID        int64  `json:"msgId"`
		SenderID     int64  `json:"senderId"`
		Ver          string `json:"ver"`
		Data         struct {
			DeviceID string `json:"deviceId"`
			Cmd      int    `json:"cmd"`
			Ver      string `json:"ver"`
			Data     struct {
				Cmd    int     `json:"cmd"`
				Ver    string  `json:"ver"`
				Width  int     `json:"width"`
				Height int     `json:"height"`
				FPS    float64 `json:"fps"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}

	if env.DeviceID != "PC-DEVICE-ID==" {
		t.Errorf("envelope deviceId = %q", env.DeviceID)
	}
	// serviceId is the routing key (must be 1001); srcServiceId is the
	// listener id the phone will answer on.
	if env.ServiceID != 1001 {
		t.Errorf("serviceId = %d, want 1001", env.ServiceID)
	}
	if env.SrcServiceID != 15 {
		t.Errorf("srcServiceId = %d, want the listener id 15", env.SrcServiceID)
	}
	if env.SenderID != senderIDReadyForAssist {
		t.Errorf("senderId = %d, want %d", env.SenderID, senderIDReadyForAssist)
	}
	if env.MsgID <= 0 {
		t.Errorf("msgId = %d, want > 0", env.MsgID)
	}
	if env.Ver != "1.0" {
		t.Errorf("ver = %q", env.Ver)
	}
	if env.Data.DeviceID != "PC-DEVICE-ID==" {
		t.Errorf("FeaturesServiceRequest.deviceId = %q, want the pc id", env.Data.DeviceID)
	}
	if env.Data.Cmd != 11 {
		t.Errorf("FeaturesServiceRequest.cmd = %d, want 11", env.Data.Cmd)
	}
	if env.Data.Data.Cmd != castMirror {
		t.Errorf("CastPhoneReqData.cmd = %d, want %d", env.Data.Data.Cmd, castMirror)
	}
	if env.Data.Data.Width != 1920 || env.Data.Data.Height != 1080 || env.Data.Data.FPS != 30 {
		t.Errorf("mode = %dx%d@%g", env.Data.Data.Width, env.Data.Data.Height, env.Data.Data.FPS)
	}
}

// cmd 1000 registers the listener id (services/a.B: :cond_29a adds
// WsData.srcServiceId to the HashSet). It carries no CastPhoneReqData.
func TestCastRegisterShape(t *testing.T) {
	oldInfo, oldMode := ourInfo, castDeviceIDMode
	defer func() { ourInfo, castDeviceIDMode = oldInfo, oldMode }()
	ourInfo.DeviceID = "PC-ID=="
	castDeviceIDMode = "device"

	b, err := buildCastRegister("")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var env struct {
		SrcServiceID int `json:"srcServiceId"`
		ServiceID    int `json:"serviceId"`
		Data         struct {
			Cmd  int `json:"cmd"`
			Data any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	if env.ServiceID != 1001 || env.SrcServiceID != castListen {
		t.Errorf("ids = %d/%d, want 1001/%d", env.ServiceID, env.SrcServiceID, castListen)
	}
	if env.Data.Cmd != 1000 {
		t.Errorf("register cmd = %d, want 1000", env.Data.Cmd)
	}
	if env.Data.Data != nil {
		t.Errorf("register payload = %v, want null", env.Data.Data)
	}
}

func TestCastModeCmd(t *testing.T) {
	cases := map[string]int{
		"mirror":    castMirror,
		"desktop":   castDesktop,
		"mobileui":  castMobileUI,
		"mobile-ui": castMobileUI,
		"stop":      castStop,
	}
	for mode, want := range cases {
		got, ok := castModeCmd(mode)
		if !ok || got != want {
			t.Errorf("castModeCmd(%q) = %d,%v want %d", mode, got, ok, want)
		}
	}
	if _, ok := castModeCmd("nonsense"); ok {
		t.Error("castModeCmd(nonsense) unexpectedly ok")
	}
}

// -castdevice=real must put the phone's realDeviceId in the lookup field while
// leaving the envelope id as our own.
func TestCastDeviceReal(t *testing.T) {
	oldMode := castDeviceIDMode
	defer func() { castDeviceIDMode = oldMode }()
	castDeviceIDMode = "real"
	b, err := buildCastRequest("stop", "PHONE-REAL-ID==")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var env struct {
		DeviceID string `json:"deviceId"`
		Data     struct {
			DeviceID string `json:"deviceId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.DeviceID != "PHONE-REAL-ID==" {
		t.Errorf("lookup deviceId = %q, want the real id", env.Data.DeviceID)
	}
}
