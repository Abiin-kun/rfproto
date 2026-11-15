// PC → phone screen share request (serviceId 202, PC_SCREENSHARE).
//
// Recovered from services/WebView.startScreenShare (0x1808aae40). That function
// builds the payload with a literal format string
//
//	{"shareType":"%s","setAudio":true,"setScale":150,"setPosX":0,"setPosY":0,"deviceid":"%s"}
//
// and hands it to SendServiceMsgByMap, whose service id is the immediate loaded
// right before the call:
//
//	1808ab06f  mov  $0xca,%ecx          ; 0xca = 202 = PC_SCREENSHARE
//	1808ab077  call SendServiceMsgByMap  ; 0x1807a3c80
//
// 202 is corroborated from the phone's own side: in its device-status frame it
// advertises `"cap":{"…","202":2,…}` (capture/c1 frame 007, docs/03 §8.3), i.e.
// exactly the service we are about to drive.
//
// The share-type literals in that function are `mirror` and `extend`, alongside
// `MIRROR`/`EXTEND` and the tokens `height`, `Webcam`, `notifyaction`, `photos`,
// `sc_parlpsu` — mirror and extend (second display) are the same message family.
//
// This is only the *control* half: the real PC hands the video path to a
// separate native process (plugins\ScreenSharePlugin\ScreenSharePlugin.exe,
// screenshare\ScreenShareService.exe) which owns the driver and the codec. See
// docs/03 §10 for what follows this message.
//
// The frame's exact shape was recovered from the call site (docs/03 §10, E25):
// SendServiceMsgByMap json.Marshals a map with keys "cmd"=25, "ver"="1" and
// "data"=<the format string's result>, so `data` is a JSON *string*. The
// envelope deviceId is the sender's own id (SendMessage's own-id default for
// serviceId >= 99), and msgId is ++globalMsgId.
package main

import (
	"encoding/json"
	"log"
	"sync"
)

// svcPCScreenShare is the service id these messages belong to, taken from the
// immediate in services/WebView.startScreenShare. Other ids it sits beside in
// the phone's interactions (docs/03 §8): 0 MANAGER, 17 APP_LIST, 25
// DEVICE_STATUS, 168 FILES — all of which the phone announces to us, none of
// which we drive yet.
const svcPCScreenShare = 202

// The wire payload is not the format string alone: SendServiceMsgByMap
// json.Marshals a map[string]any, and startScreenShare fills that map with
// three keys before calling it (all three recovered from the call site):
//
//	0x1808aaf6d  key "ver"  (bytes 0x180b8eee9) = *(*string)0x180d160d0 = "1"
//	0x1808aafbf  key "cmd"  (bytes 0x180b8eebf) = *(*int)0x180d15418    = 25
//	0x1808ab01d  key "data" (bytes 0x180b8f553) = fmt.Sprintf(fmtString, shareType, deviceid)
//
// json.Marshal sorts map keys, so the emitted payload is
// {"cmd":25,"data":"<share json>","ver":"1"} — i.e. `data` is a JSON *string*,
// not an object. Sending the share fields as a nested object (as this
// prototype did first) is not what the PC sends.
const (
	pcScreenShareCmd = 25
	pcScreenShareVer = "1"
)

// senderId selects the phone subsystem, not whether the sender is a PC.
// Ordinary mobile-desktop services use 0; Ready For Assist traffic uses 1.
const (
	senderIDMobileDesktop  = 0
	senderIDReadyForAssist = 1
)

// screenShareRequest is the inner payload (the string under `data`), in the
// order of the recovered format string.
type screenShareRequest struct {
	ShareType string `json:"shareType"`
	SetAudio  bool   `json:"setAudio"`
	SetScale  int    `json:"setScale"`
	SetPosX   int    `json:"setPosX"`
	SetPosY   int    `json:"setPosY"`
	DeviceID  string `json:"deviceid"` // note: lowercase, as in the format string
}

// screenSharePayload mirrors json.Marshal(map[string]any{"cmd":…,"data":…,"ver":…}).
type screenSharePayload struct {
	Cmd  int    `json:"cmd"`
	Data string `json:"data"`
	Ver  string `json:"ver"`
}

// screenShareMsg is the outer websocket.WebsocketMsg.
//
//   - deviceId is the *sender's* own id, not the target. websocket.SendMessage
//     (0x1807a3f40) replaces an empty id with device.GetDeviceId() when the
//     service id is >= 99 or == 8 (0x1807a4016/0x1807a4020) and then normalises
//     it through device.GetActualDeviceId (0x1807a404b); startScreenShare passes
//     GetDeviceId() explicitly (0x1808ab068). The peer is addressed by the
//     WebView service registration, not by this field.
//   - msgId is ++globalMsgId, an atomic counter (lock xadd at 0x1807a3ff7); it
//     is never 0 on the wire.
//   - srcServiceId == serviceId, as SendServiceMsgByMap is addressed by one id.
type screenShareMsg struct {
	DeviceID     string              `json:"deviceId,omitempty"`
	SrcServiceID int                 `json:"srcServiceId"`
	ServiceID    int                 `json:"serviceId"`
	MsgID        int64               `json:"msgId,omitempty"`
	SenderID     int64               `json:"senderId"`
	Data         *screenSharePayload `json:"data,omitempty"`
}

// wireMsgID is the ++globalMsgId analogue (websocket.SendMessage's counter).
var (
	wireMsgIDMu sync.Mutex
	wireMsgID   int64
)

func nextWireMsgID() int64 {
	wireMsgIDMu.Lock()
	defer wireMsgIDMu.Unlock()
	wireMsgID++
	return wireMsgID
}

// screenShareTypes are the values services/WebView.startScreenShare accepts.
var screenShareTypes = map[string]bool{"mirror": true, "extend": true}

// shareIDMode is -shareid: "device" (default) or "real". It selects which of the
// phone's two ids shareTarget puts in `data.deviceid`.
var shareIDMode = "device"

// buildScreenShareRequest assembles the service-202 request. peerID is the
// *phone's* id: it is what goes in `data.deviceid`, the same string passed to
// services/WebView.Connect/Disconnect as the device the service is opened for
// (0x1808aaeaa/0x1808aae85). The envelope deviceId is our own id instead —
// SendMessage's own-id default for serviceId >= 99, above.
func buildScreenShareRequest(peerID, shareType string) ([]byte, error) {
	inner, err := json.Marshal(&screenShareRequest{
		ShareType: shareType,
		SetAudio:  true,
		SetScale:  150,
		SetPosX:   0,
		SetPosY:   0,
		DeviceID:  peerID,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(&screenShareMsg{
		DeviceID:     ourInfo.DeviceID,
		SrcServiceID: svcPCScreenShare,
		ServiceID:    svcPCScreenShare,
		MsgID:        nextWireMsgID(),
		SenderID:     senderIDReadyForAssist,
		Data: &screenSharePayload{
			Cmd:  pcScreenShareCmd,
			Data: string(inner),
			Ver:  pcScreenShareVer,
		},
	})
}

// shareTarget picks the phone id that goes in the 202 payload's `data.deviceid`
// (and that the real PC would hand to services/WebView.Connect). -shareid
// chooses between the phone's envelope `deviceId` (the changeable id it presents
// in its hello) and its `realDeviceId` (the stable one it uses inside service
// payloads, e.g. the NFS handshake and the file channel). The real PC normalises
// ids through device.GetActualDeviceId (0x180306c40) before using them, so
// `real` is the faithful default candidate when the two differ.
func shareTarget(deviceID, realID string) string {
	if shareIDMode == "real" && realID != "" {
		return realID
	}
	if deviceID != "" {
		return deviceID
	}
	return realID
}

// screenShareFor returns the frame to send after the pair-ack, per -share.
// deviceID/realID are the phone's ids (learned from its own frames); the one
// chosen by -shareid is both the envelope address and the `data.deviceid` target.
func screenShareFor(deviceID, realID string) ([]byte, string) {
	if shareOnPair == "" || shareOnPair == "off" {
		return nil, "share(off)"
	}
	if !screenShareTypes[shareOnPair] {
		log.Printf("[share] unknown -share %q (want mirror|extend)", shareOnPair)
		return nil, "share(bad)"
	}
	peerID := shareTarget(deviceID, realID)
	if peerID == "" {
		log.Printf("[share] phone id not learned yet; using our id as the data.deviceid target")
		peerID = ourInfo.DeviceID
	}
	b, err := buildScreenShareRequest(peerID, shareOnPair)
	if err != nil {
		log.Printf("[share] build: %v", err)
		return nil, "share(error)"
	}
	return b, "screenshare(" + shareOnPair + ")"
}

// peerDeviceID extracts the sender's own device id from a received frame. The
// phone describes itself unambiguously in data.device (deviceId + realDeviceId);
// the envelope deviceId is not reliable for this — the phone sets it to the
// *peer* on the pair-confirm (capture/c1/002 carries our id) but to itself on
// its service-init frames (001/003/004).
func peerDeviceID(msg []byte) string {
	var env struct {
		DeviceID string `json:"deviceId"`
		Data     struct {
			Device struct {
				DeviceID string `json:"deviceId"`
			} `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return ""
	}
	if d := env.Data.Device.DeviceID; d != "" && d != ourInfo.DeviceID {
		return d
	}
	if env.DeviceID != "" && env.DeviceID != ourInfo.DeviceID {
		return env.DeviceID
	}
	return ""
}

// peerRealDeviceID extracts the phone's stable `realDeviceId` from a received
// frame. Unlike `deviceId` it does not change between sessions (E24), and it is
// the id the phone itself uses inside service payloads — so it is the faithful
// form of `data.deviceid` for -shareid real.
func peerRealDeviceID(msg []byte) string {
	var env struct {
		Data struct {
			Device struct {
				DeviceID     string `json:"deviceId"`
				RealDeviceID string `json:"realDeviceId"`
			} `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return ""
	}
	d := env.Data.Device
	if d.RealDeviceID != "" && d.RealDeviceID != d.DeviceID {
		return d.RealDeviceID
	}
	return ""
}
