// probe.go: PC -> phone messages used to answer one question that the inert
// serviceId-202 request left open (docs/INVESTIGATION.md E28/E29, U9):
//
//	Is the phone's *feature*-message dispatcher gated on device readiness, or is
//	serviceId 202 specifically inert?
//
// We have never observed the phone act on a non-manager frame from the PC. The
// only proven PC -> phone effect is the manager (serviceId 0) hello reply, and
// that is literally what moves the phone from its hello to the pair-confirm. So
// a global gate and a 202-specific dead end look identical from the wire alone.
//
// The probes below are deliberately cheap and multi-pronged; the phone's logcat
// is the oracle (a reaction to *any* of them proves dispatch is live; uniform
// silence across several feature services supports the global-gate reading):
//
//	-statusreply   answer the phone's DEVICE_STATUS (25) bring-up with the PC's
//	               own status, using the same key set the phone uses for its own
//	               status frame (capture/c8/008) so it parses cleanly. This is
//	               also the leading candidate for the missing readiness step.
//	-viewonline    send a PC_WEBVIEW (100) frame after the pair-ack.
//	-echosvc S,... replay selected phone frames back to it verbatim-modulo-
//	               envelope (schema-free: no guessing at all).
//	-fsreply       answer the file-station (168) handshake. Best-effort: the PC
//	               half is VaultPlugin + a Dokan/NFS server (E28), so a message
//	               alone may not be enough.
//
// Every payload here is built from *observed phone frames* or from the recovered
// device maps in reply.go. Nothing is copied from the target; the schemas are
// reconstructions from wire observations and disassembly.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Service ids used by the probes. They are the ones the phone itself drives
// during bring-up (capture/c8 frames 004..015), plus the file-station channel.
const (
	svcDeviceStatus = 25
	svcClipboard    = 9
	svcInput        = 16
	svcPCWebView    = 100
	svcAccount      = 32
	svcFiles        = 168
)

// Probe configuration, set from the serve flags.
var (
	probeStatusReply bool
	probeViewOnline  bool
	probeEchoSvcs    map[int]bool
	probeFSReply     string
	probeFSSvc       = svcFiles
	probeFSCmd       = 1001
	probeViewDelay   = 3 * time.Second
)

// probeMinGap throttles each probe kind so a phone that retries every second
// (the file-station handshake does exactly that) does not turn into a flood.
const probeMinGap = 900 * time.Millisecond

// probeSession is per-WebSocket-session probe state.
type probeSession struct {
	last map[string]time.Time
	// peerHost is the phone's TCP address on the ws connection. It is the
	// fallback host for the app-icon HTTPS probe (appicons.go) when
	// -appiconhost is not set.
	peerHost string
}

// parseSvcSet parses -echosvc ("25,9,16") into a set.
func parseSvcSet(s string) map[int]bool {
	out := map[int]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			log.Printf("[probe] bad -echosvc entry %q: %v", f, err)
			continue
		}
		out[n] = true
	}
	return out
}

func newProbeSession(peerHost string) *probeSession {
	return &probeSession{last: map[string]time.Time{}, peerHost: peerHost}
}

func (p *probeSession) allow(key string) bool {
	now := time.Now()
	if t, ok := p.last[key]; ok && now.Sub(t) < probeMinGap {
		return false
	}
	p.last[key] = now
	return true
}

// svcMsg is a generic websocket.WebsocketMsg envelope for probe traffic. Field
// order and tags mirror the recovered struct in screenshare.go (msgId and
// senderId are always emitted by the PC).
type svcMsg struct {
	DeviceID     string `json:"deviceId,omitempty"`
	SrcServiceID int    `json:"srcServiceId"`
	ServiceID    int    `json:"serviceId"`
	MsgID        int64  `json:"msgId"`
	SenderID     int64  `json:"senderId"`
	Data         any    `json:"data"`
}

// envelopeOf parses just the routing fields of an inbound frame.
func envelopeOf(msg []byte) (serviceID int, cmd *int, ok bool) {
	var outer struct {
		ServiceID int             `json:"serviceId"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(msg, &outer) != nil {
		return 0, nil, false
	}
	var inner struct {
		Cmd *int `json:"cmd"`
	}
	if len(outer.Data) > 0 {
		_ = json.Unmarshal(outer.Data, &inner)
	}
	return outer.ServiceID, inner.Cmd, true
}

// inbound reacts to one received frame with whichever probes are enabled.
// peerRealID is the phone's stable realDeviceId once learned (used as the target
// inside the file-station payload).
func (p *probeSession) inbound(seq int, msg []byte, peerRealID string, send func(kind string, out []byte)) {
	sid, cmd, ok := envelopeOf(msg)
	if !ok {
		return
	}

	if probeStatusReply && sid == svcDeviceStatus && p.allow("status") {
		if out, err := buildPCStatus(); err != nil {
			log.Printf("[probe] status build: %v", err)
		} else {
			log.Printf("[probe] #%d serviceId=%d -> PC DEVICE_STATUS reply (%dB)", seq, sid, len(out))
			send("status", out)
		}
	}

	if probeFSReply != "" && sid == probeFSSvc && cmd != nil && *cmd == probeFSCmd && p.allow("fs") {
		if out, err := buildFileStationReply(*cmd, peerRealID); err != nil {
			log.Printf("[probe] fs reply build: %v", err)
		} else {
			log.Printf("[probe] #%d serviceId=%d cmd=%d -> file-station reply svc=%d (%dB)",
				seq, sid, *cmd, probeFSSvc, len(out))
			send("fsreply", out)
		}
	}

	if probeEchoSvcs[sid] && p.allow(fmt.Sprintf("echo%d", sid)) {
		if out, err := buildEcho(sid, msg); err != nil {
			log.Printf("[probe] echo build: %v", err)
		} else {
			log.Printf("[probe] #%d serviceId=%d -> echo back (%dB)", seq, sid, len(out))
			send(fmt.Sprintf("echo(%d)", sid), out)
		}
	}

	// The phone advertises its HTTPS service (port/token/cert fingerprint) in
	// the service-17 app list. See appicons.go.
	if appIconsOn && sid == svcAppList && cmd != nil && *cmd == 1 && p.allow("appicons") {
		p.probeAppList(seq, msg)
	}
}

// buildEcho replays the phone's own frame back to it, re-addressed as a PC
// frame: our own deviceId (E25), a fresh msgId and senderId 1. Everything else,
// notably `data`, is passed through untouched, so no schema guess is involved.
func buildEcho(sid int, msg []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, err
	}
	m["deviceId"] = ourInfo.DeviceID
	m["msgId"] = nextWireMsgID()
	m["senderId"] = senderIDReadyForAssist
	return json.Marshal(m)
}

// pcStatus is the DEVICE_STATUS payload the PC sends. The field set, order and
// json tags are taken verbatim from the phone's own full status frame
// (capture/c8/008, serviceId 25 cmd 1) — the two peers share the struct.
type pcStatus struct {
	AiCapabilities []any       `json:"AiCapabilities"`
	AccessAllFiles int         `json:"accessAllFiles"`
	AccountID      int         `json:"accountId"`
	Adb            bool        `json:"adb"`
	Battery        int         `json:"battery"`
	Bluetooth      bool        `json:"bluetooth"`
	Cap            map[int]int `json:"cap"`
	ChargePlug     int         `json:"chargePlug"`
	Data           bool        `json:"data"`
	DataSignal     int         `json:"dataSignal"`
	DeviceName     string      `json:"deviceName"`
	DeviceType     int         `json:"deviceType"`
	FoldState      int         `json:"foldState"`
	FoldType       int         `json:"foldType"`
	IsCharging     bool        `json:"isCharging"`
	Lite           bool        `json:"lite"`
	Model          string      `json:"model"`
	RealDeviceName string      `json:"realDeviceName"`
	Screen         int         `json:"screen"`
	USBReady       bool        `json:"usbReady"`
	WallpaperState int         `json:"wallpaperState"`
	Wifi           bool        `json:"wifi"`
	WifiSignal     int         `json:"wifiSignal"`
	Cmd            int         `json:"cmd"`
	Ver            string      `json:"ver"`
}

// buildPCStatus assembles the PC's DEVICE_STATUS reply on serviceId 25.
//
// The command value 1 is the phone's own "full status" command; the phone first
// sends cmd 20 (the no-screen variant) and then cmd 1, and the PC answers with
// its own status. We reply to any serviceId-25 frame with cmd 1.
func buildPCStatus() ([]byte, error) {
	battery, charging := pcBattery()
	model := ourInfo.SKU
	if model == "" {
		model = ourInfo.Name
	}
	data := &pcStatus{
		AiCapabilities: []any{},
		Adb:            false,
		Battery:        battery,
		Bluetooth:      false,
		Cap:            pcCapabilities,
		ChargePlug:     boolToInt(charging),
		Data:           false,
		DeviceName:     ourInfo.Name,
		DeviceType:     4, // device.Device+0x30 Type = 4 (PC), same as ourDevice
		IsCharging:     charging,
		Model:          model,
		RealDeviceName: ourInfo.Name,
		Screen:         1,
		WallpaperState: 0,
		Wifi:           true,
		WifiSignal:     4,
		Cmd:            1,
		Ver:            "1",
	}
	return json.Marshal(&svcMsg{
		DeviceID:     ourInfo.DeviceID,
		SrcServiceID: svcDeviceStatus,
		ServiceID:    svcDeviceStatus,
		MsgID:        nextWireMsgID(),
		SenderID:     senderIDMobileDesktop,
		Data:         data,
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pcBattery reads the host battery, the same kind of value getDeviceStatusMsgBytes
// fills for the PC. A desktop with no battery reports 0/not-charging.
func pcBattery() (pct int, charging bool) {
	bat, _ := filepath.Glob("/sys/class/power_supply/BAT*")
	if len(bat) == 0 {
		return 0, false
	}
	if b, err := os.ReadFile(filepath.Join(bat[0], "capacity")); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			pct = n
		}
	}
	if s, err := os.ReadFile(filepath.Join(bat[0], "status")); err == nil {
		switch strings.TrimSpace(string(s)) {
		case "Charging", "Full":
			charging = true
		}
	}
	return pct, charging
}

// pcWebViewMsgTarget documents the phone's PC_WEBVIEW (100) frame shape
// (capture/c8/006: cmd 1018 + notification counters). The cmd vocabulary on
// service 100 is large and only 1018 has been observed, so this probe is the
// weakest of the set — -echosvc 100 (a verbatim replay) is the better test.

// buildViewOnline announces the PC's PC_WEBVIEW state. services/WebView
// .updateOnlineState (0x1808a2260) tracks which of the view services
// {1,2,3,9,13,15,20,26,202} are open as a per-device bitmask (202 -> 0x100) and
// sends on serviceId 100 (SendWebViewMsg 0x1808b9bc0 also sends with ecx=0x64).
// The cmd for that message was not recovered, so this sends the one observed
// service-100 shape.
func buildViewOnline() ([]byte, error) {
	return json.Marshal(&svcMsg{
		DeviceID:     ourInfo.DeviceID,
		SrcServiceID: svcPCWebView,
		ServiceID:    svcPCWebView,
		MsgID:        nextWireMsgID(),
		SenderID:     senderIDReadyForAssist,
		Data: map[string]any{
			"cmd":        1018,
			"enabled":    true,
			"granted":    true,
			"triggered":  false,
			"unreadCnt":  0,
			"unreadList": []any{map[string]any{"ids": []any{}, "type": 1}, map[string]any{"ids": []any{}, "type": 2}},
			"ver":        "5.0",
		},
	})
}

// fileStationMsg mirrors the payload the phone sends on serviceId 168
// (capture/c8/009..011): {cmd, data:"<json string>", ver:"1.0"}.
type fileStationMsg struct {
	Cmd  int    `json:"cmd"`
	Data string `json:"data"`
	Ver  string `json:"ver"`
}

// buildFileStationReply answers the phone's NFS handshake (serviceId 168, cmd
// 1001) with a best-effort serverInfo. -fsreply "auto" supplies a plausible
// payload from VaultPlugin.exe's recovered keys (nfsport/nfsDataPort/
// capability/nfs_capability); any other value is used verbatim as the inner
// JSON. The genuinely faithful reply needs the plugin's own serverInfo, and in
// the real PC it is backed by a running NFS server (E28).
func buildFileStationReply(cmd int, peerRealID string) ([]byte, error) {
	inner := probeFSReply
	if inner == "auto" {
		m := map[string]any{
			"type":           0,
			"path":           "",
			"port":           0,
			"nfsport":        2049,
			"nfsDataPort":    2049,
			"capability":     1,
			"nfs_capability": 24,
			"nfs_version":    8,
			"nfs_dependency": 24,
			"timestamp":      time.Now().UnixMilli(),
			"dt2type":        3,
			"deviceId":       peerRealID,
		}
		if ourInfo.FP != "" {
			m["fp"] = ourInfo.FP
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		inner = string(b)
	}
	if inner == "" {
		return nil, fmt.Errorf("empty -fsreply")
	}
	return json.Marshal(&svcMsg{
		DeviceID:     ourInfo.DeviceID,
		SrcServiceID: probeFSSvc,
		ServiceID:    probeFSSvc,
		MsgID:        nextWireMsgID(),
		SenderID:     senderIDReadyForAssist,
		Data: &fileStationMsg{
			Cmd:  cmd,
			Data: inner,
			Ver:  "1.0",
		},
	})
}
