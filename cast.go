// cast.go: diagnostic control of the phone's receiving-side cast client.
//
// This is NOT the phone-desktop producer. Phone -> Linux uses services 1/15,
// implemented in streaming.go and the local browser workspace.
//
// Evidence (docs/INVESTIGATION.md E37, from mobiledesktop-base.apk):
//
//	com.moto.smartconnect.plugin.bean.CastPhoneReqData
//	    CMD_CAST_MIRROR   = 0
//	    CMD_CAST_DESKTOP  = 1
//	    CMD_CAST_MOBILEUI = 2
//	    CMD_CAST_STOP     = 100 (0x64)
//	    fields: cmd, ver, width, height, fps
//
//	com.moto.smartconnect.plugin.bean.CastPhoneStateData
//	    fields: cmd, castMode, connectState, width, height, fps, ver
//
//	com.motorola.smartconnect.connectivity.services.a  (SCBaseServiceHelper)
//	    super(context, 0x3e9)                      -> wire serviceId 1001
//	    SCBaseServiceHelper.preHandleMessage        -> dispatches only when
//	                                                   WsData.serviceId == 1001
//	    services/a.B (the FeaturesServiceRequest switch):
//	        cmd == 1000 (0x3e8) -> listener set.add(WsData.srcServiceId)
//	        cmd == 1001 (0x3e9) -> listener set.remove(...)   (a.W)
//	        cmd == 11   (0x0b)  -> CastPhoneReqData
//	                               -> FeaturesServiceHelper.H(deviceId, req)
//	                               H logs "castPhone, castCmd: %s, deviceId: %s" and
//	                               for 0/1/2 starts ReadyForService.M(cmd,
//	                               MotoDisplayMode(width,height,fps), deviceId);
//	                               for 0x64 calls ReadyForService.C(deviceId).
//
// Envelope is Gson over WsData{deviceId, srcServiceId, serviceId, msgId,
// senderId, ver, data} (pm5.h0 is a plain Gson.toJson, so keys == field names).
// The payload is FeaturesServiceRequest{deviceId, cmd, ver, data} wrapping
// CastPhoneReqData.
//
// Direction matters: the phone sends its state back as
// WsData{srcServiceId: 1001, serviceId: <our registered listener id>,
//
//	data: CastPhoneStateData} (services/a.mh3 does setServiceId() while
//
// iterating that same listener set). So a cast request is only observable if
// we register a listener id first: that is what cmd 1000 does.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

const (
	// svcFeatures is the phone's feature bus (SCBaseServiceHelper), built with
	// 0x3e9 in services/a.<init>.
	svcFeatures = 1001
	// castCmdRegister adds WsData.srcServiceId to the phone's listener set.
	castCmdRegister = 1000
	// castCmdUnregister removes it again.
	castCmdUnregister = 1001
	// castCmdRequest is the FeaturesServiceRequest.cmd that carries a
	// CastPhoneReqData (switch key 0xb in services/a.B).
	castCmdRequest = 11
)

// CastPhoneReqData.cmd values.
const (
	castMirror   = 0
	castDesktop  = 1
	castMobileUI = 2
	castStop     = 100
)

// castModeCmd maps the -cast value to CastPhoneReqData.cmd.
func castModeCmd(mode string) (int, bool) {
	switch mode {
	case "mirror":
		return castMirror, true
	case "desktop":
		return castDesktop, true
	case "mobileui", "mobile-ui":
		return castMobileUI, true
	case "stop":
		return castStop, true
	}
	return 0, false
}

// Cast probe configuration, set from the serve flags.
var (
	castMode         string
	castWidth        = 1920
	castHeight       = 1080
	castFPS          = 30.0
	castVersion      = "1.0"
	castServiceID    = svcFeatures
	castOuterCmd     = castCmdRequest
	castDelay        time.Duration
	castRepeat       = 1
	castRepeatGap    = 3 * time.Second
	castDeviceIDMode = "device" // "device" (envelope id the phone knows) or "real"
	castListen       = 15       // listener id we register; the phone echoes state here
	castRegister     = true
	castRegGap       = 400 * time.Millisecond
)

// featuresEnvelope mirrors WsData (com/motorola/mobiledesktop/connectivity/bean/WsData):
// data, deviceId, msgId, senderId, serviceId, srcServiceId, ver.
type featuresEnvelope struct {
	DeviceID     string `json:"deviceId,omitempty"`
	SrcServiceID int    `json:"srcServiceId"`
	ServiceID    int    `json:"serviceId"`
	MsgID        int64  `json:"msgId"`
	SenderID     int64  `json:"senderId"`
	Ver          string `json:"ver,omitempty"`
	Data         any    `json:"data"`
}

// featuresRequest mirrors com/moto/smartconnect/plugin/bean/FeaturesServiceRequest:
// its own deviceId + data, plus cmd/ver inherited from BaseServiceData.
type featuresRequest struct {
	DeviceID string `json:"deviceId"`
	Cmd      int    `json:"cmd"`
	Ver      string `json:"ver,omitempty"`
	Data     any    `json:"data"`
}

// castPhoneReq mirrors com/moto/smartconnect/plugin/bean/CastPhoneReqData.
type castPhoneReq struct {
	Cmd    int     `json:"cmd"`
	Ver    string  `json:"ver"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	FPS    float64 `json:"fps"`
}

// castTargetID is the PC id the phone looks up in services/a.H. Our hello
// advertises the same value as deviceId and realDeviceId, so "device" is
// normally correct; "real" exists to A/B the lookup if H bails out early.
func castTargetID(peerRealID string) string {
	if castDeviceIDMode == "real" && peerRealID != "" {
		return peerRealID
	}
	return ourInfo.DeviceID
}

// buildFeaturesFrame assembles one service-1001 frame. srcID is what the phone
// records as a listener id on cmd 1000, and what it addresses state replies to.
func buildFeaturesFrame(outerCmd int, payload any, srcID int, peerRealID string) ([]byte, error) {
	env := featuresEnvelope{
		DeviceID:     ourInfo.DeviceID,
		SrcServiceID: srcID,
		ServiceID:    castServiceID,
		MsgID:        nextWireMsgID(),
		SenderID:     senderIDReadyForAssist,
		Ver:          castVersion,
		Data: featuresRequest{
			DeviceID: castTargetID(peerRealID),
			Cmd:      outerCmd,
			Ver:      castVersion,
			Data:     payload,
		},
	}
	return json.Marshal(env)
}

// buildCastRequest assembles the cast request itself (outer cmd 11).
func buildCastRequest(mode, peerRealID string) ([]byte, error) {
	inner, ok := castModeCmd(mode)
	if !ok {
		return nil, fmt.Errorf("unknown cast mode %q", mode)
	}
	return buildFeaturesFrame(castOuterCmd, castPhoneReq{
		Cmd:    inner,
		Ver:    castVersion,
		Width:  castWidth,
		Height: castHeight,
		FPS:    castFPS,
	}, castListen, peerRealID)
}

// buildCastRegister assembles the cmd-1000 listener registration.
func buildCastRegister(peerRealID string) ([]byte, error) {
	return buildFeaturesFrame(castCmdRegister, nil, castListen, peerRealID)
}

// castLog describes a frame for the serve log.
func castLog(label string, b []byte) string {
	return fmt.Sprintf("%s (svc %d, listener %d, %dB): %s", label, castServiceID, castListen, len(b), clip(b, 600))
}

// sendCast registers a listener id, then sends the cast request (optionally
// repeated), then unregisters after a stop. Mirrors the screen-share sender:
// one goroutine, writes funnelled through send().
func sendCast(peerRealID string, send func(kind string, out []byte)) {
	inner, _ := castModeCmd(castMode)
	reps := castRepeat
	if reps < 1 {
		reps = 1
	}
	go func() {
		if castMode == "stop" {
			// Stop only needs a target; no state to watch.
		} else if castRegister {
			if b, err := buildCastRegister(peerRealID); err != nil {
				log.Printf("[cast] register build: %v", err)
			} else {
				log.Printf("[cast] %s", castLog("register listener", b))
				send("cast-register", b)
				time.Sleep(castRegGap)
			}
		}
		for i := 0; i < reps; i++ {
			if castDelay > 0 {
				time.Sleep(castDelay)
			}
			b, err := buildCastRequest(castMode, peerRealID)
			if err != nil {
				log.Printf("[cast] build: %v", err)
				return
			}
			log.Printf("[cast] %s (inner cmd %d %dx%d@%g)",
				castLog("cast "+castMode, b), inner, castWidth, castHeight, castFPS)
			send("cast", b)
			if i < reps-1 {
				time.Sleep(castRepeatGap)
			}
		}
		if castMode == "stop" {
			time.Sleep(300 * time.Millisecond)
			if b, err := buildFeaturesFrame(castCmdUnregister, nil, castListen, peerRealID); err == nil {
				log.Printf("[cast] %s", castLog("unregister listener", b))
				send("cast-unregister", b)
			}
		}
	}()
}
