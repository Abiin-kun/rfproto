package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type recordedStreamMessage struct {
	ServiceID int `json:"serviceId"`
	Data      struct {
		Cmd        int             `json:"cmd"`
		InstanceID int32           `json:"instanceId"`
		MessageID  uint64          `json:"messageId"`
		Data       json.RawMessage `json:"data"`
	} `json:"data"`
}

func streamingFixture(t *testing.T) (*viewerHub, *liveSession, *[]recordedStreamMessage, func()) {
	t.Helper()
	var sent []recordedStreamMessage
	control := &liveSession{name: "Test phone", send: func(_ string, body []byte) error {
		var message recordedStreamMessage
		if err := json.Unmarshal(body, &message); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, message)
		return nil
	}}
	h := &viewerHub{control: control, catalog: make(map[string]catalogApp), sessions: make(map[string]*streamSession), instance: 123, done: make(chan struct{})}
	_, cancel, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return h, control, &sent, cancel
}

func receiveStream(t *testing.T, h *viewerHub, control *liveSession, service, cmd int, data any) {
	t.Helper()
	body, err := json.Marshal(struct {
		Data streamCommandData `json:"data"`
	}{streamCommandData{Ver: "1", Cmd: cmd, Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	h.inbound(control, service, body)
}

func TestStreamingReusesDisplayWithoutRenegotiating(t *testing.T) {
	h, control, _, _ := streamingFixture(t)
	first := catalogApp{Pkg: "example.first", Cmp: "example.first.Main", Name: "First"}
	second := catalogApp{Pkg: "example.second", Cmp: "example.second.Main", Name: "Second"}
	h.catalog[first.id()], h.catalog[second.id()] = first, second
	h.appLimit = 1
	session := h.newSessionLocked("app", first, first.Name, 1280, 720)
	session.State, session.DisplayID, session.MediaID = "connected", 8, "existing-video"
	// Above the JavaScript integer precision boundary: only Go owns this ID.
	session.wireID = 9007199254741001
	result, err := h.command(viewerCommand{Op: "launch", Mode: "app", AppID: second.id(), Width: 1280, Height: 720})
	if err != nil {
		t.Fatalf("a full display pool must still allow phone-side reuse: %v", err)
	}
	pendingID := result.(map[string]string)["sessionId"]
	receiveStream(t, h, control, svcAppStream, 12, map[string]any{
		"pkg": second.Pkg, "cmp": second.Cmp, "userId": 0, "displayId": 8,
	})
	state := h.state()
	if len(state.Sessions) != 1 || h.sessions[pendingID] != nil || state.Sessions[0].ID != session.ID ||
		state.Sessions[0].Name != second.Name || state.Sessions[0].MediaID != "existing-video" {
		t.Fatalf("merge must focus/relabel the existing display, not create/stop its transport: %+v", state.Sessions)
	}
	receiveStream(t, h, control, svcAppStream, 3, map[string]any{
		"sessionId": session.wireID, "displayId": 8, "width": 720, "height": 1280,
	})
	state = h.state()
	if state.Sessions[0].State != "connected" || state.Sessions[0].MediaID != "existing-video" ||
		state.Sessions[0].Width != 720 || state.Sessions[0].Height != 1280 || session.wireID != 9007199254741001 {
		t.Fatalf("format-only descriptor must preserve media and exact wire identity: %+v", state.Sessions[0])
	}
}

func TestStreamingViewerCloseReleasesInputAndRemoteResources(t *testing.T) {
	h, _, sent, cancel := streamingFixture(t)
	s := h.newSessionLocked("desktop", catalogApp{Pkg: "desktop"}, "Mobile desktop", 1280, 720)
	s.State, s.DisplayID, s.Touch, s.wireID, s.MediaID = "connected", 7, true, 987654321, "desktop-video"
	h.audio = &audioSession{viewerAudio: viewerAudio{ID: "phone-audio", State: "connected"}, wireID: 123456789, requestID: 5}
	for _, input := range []viewerInput{
		{Type: "key", Key: 29, Down: true},
		{Type: "button", Key: 1, Down: true, X: 400, Y: 300},
		{Type: "touch", PointerID: 2, Event: 1, X: 800, Y: 400, Pressure: .5},
	} {
		if _, err := h.command(viewerCommand{Op: "input", SessionID: s.ID, Input: &input}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	keyUp, buttonUp, touchCancel, videoStop, desktopStop, audioStop := -1, -1, -1, -1, -1, -1
	var lastInputID uint64
	for i, message := range *sent {
		if message.ServiceID == svcInput {
			if message.Data.InstanceID != 123 || message.Data.MessageID <= lastInputID {
				t.Fatal("input sequence lost ordering across release")
			}
			lastInputID = message.Data.MessageID
			var input streamInputData
			if err := json.Unmarshal(message.Data.Data, &input); err != nil {
				t.Fatal(err)
			}
			if input.DisplayID != 7 {
				t.Fatal("input targeted a different phone display")
			}
			switch message.Data.Cmd {
			case 3:
				if !input.Down {
					keyUp = i
				}
			case 5:
				if !input.Down {
					buttonUp = i
				}
			case 9:
				if input.Event == 3 {
					touchCancel = i
				}
			}
		}
		if message.ServiceID == svcAppStream && message.Data.Cmd == 7 {
			videoStop = i
		}
		if message.ServiceID == svcPhoneDesktop && message.Data.Cmd == 5 {
			desktopStop = i
		}
		if message.ServiceID == svcPhoneAudio && message.Data.Cmd == 5 {
			audioStop = i
		}
	}
	if keyUp < 0 || buttonUp < 0 || touchCancel < 0 || videoStop <= keyUp || videoStop <= buttonUp ||
		videoStop <= touchCancel || desktopStop < 0 || audioStop < 0 {
		t.Fatalf("viewer close must release pressed input before video stop and stop desktop/audio: key=%d mouse=%d touch=%d video=%d desktop=%d audio=%d", keyUp, buttonUp, touchCancel, videoStop, desktopStop, audioStop)
	}
	before := len(*sent)
	if _, err := h.command(viewerCommand{Op: "launch", Mode: "desktop", Width: 1280, Height: 720}); err == nil || len(*sent) != before {
		t.Fatal("queued launch recreated a session after its viewer disconnected")
	}
	if len(h.state().Sessions) != 0 {
		t.Fatal("viewer disconnect retained remote displays")
	}
}

func TestStreamingRotationKeepsPointerCoordinatesInDisplaySpace(t *testing.T) {
	h, control, sent, _ := streamingFixture(t)
	s := h.newSessionLocked("app", catalogApp{Pkg: "example.clock"}, "Clock", 1280, 720)
	s.State, s.DisplayID, s.wireID, s.MediaID = "connected", 3, 42, "clock-video"
	receiveStream(t, h, control, svcAppStream, 16, map[string]any{"sessionId": 42, "displayId": 3, "rotation": 1})
	input := viewerInput{Type: "move", X: 700, Y: 1200}
	if _, err := h.command(viewerCommand{Op: "input", SessionID: s.ID, Input: &input}); err != nil {
		t.Fatalf("rotated portrait pointer rejected: %v", err)
	}
	var wire streamInputData
	if err := json.Unmarshal((*sent)[len(*sent)-1].Data.Data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.X != 700 || wire.Y != 1200 || wire.DisplayID != 3 {
		t.Fatalf("pointer was transformed twice: %+v", wire)
	}
}

func TestDesktopRefusalReportsActualPhoneReason(t *testing.T) {
	h, control, _, _ := streamingFixture(t)
	s := h.newSessionLocked("desktop", catalogApp{Pkg: "desktop"}, "Mobile desktop", 1280, 720)
	body := []byte(`{"data":{"ver":"1.0.0","cmd":11,"port":0,"errorCode":306}}`)
	h.inbound(control, svcPhoneDesktop, body)
	if s.State != "failed" || !strings.Contains(s.Error, "306") {
		t.Fatalf("phone policy denial was lost: state=%s reason=%q", s.State, s.Error)
	}
}

func TestStreamingAcceptsPhoneCatalogSender(t *testing.T) {
	h, control, _, _ := streamingFixture(t)
	body := []byte(`{"senderId":1,"serviceId":17,"srcServiceId":17,"data":{"cmd":1,"ver":"1","appLimit":5,"apps":[{"pkg":"com.google.android.deskclock","cmp":"com.android.deskclock.DeskClock","name":"Clock","userId":0}]}}`)
	h.inbound(control, svcAppList, body)
	state := h.state()
	if state.AppLimit != 5 || len(state.Apps) != 1 || state.Apps[0].Name != "Clock" {
		t.Fatalf("phone catalog was lost: %+v", state)
	}
	app := state.Apps[0]
	if _, err := h.command(viewerCommand{Op: "launch", Mode: "app", AppID: app.ID, Width: 1280, Height: 720}); err != nil {
		t.Fatalf("received phone app could not be launched: %v", err)
	}
}

func TestPhoneAudioSignalingLifecycle(t *testing.T) {
	h, control, sent, _ := streamingFixture(t)
	h.startAudioLocked()
	a := h.audio
	request := (*sent)[len(*sent)-1]
	var params struct {
		InstanceID int32 `json:"instanceId"`
		RequestID  int32 `json:"requestId"`
		Port       int   `json:"port"`
		Protocol   int   `json:"protocol"`
		Freq       int   `json:"freq"`
		Channels   int   `json:"channels"`
		Encoding   int   `json:"encoding"`
	}
	if err := json.Unmarshal(request.Data.Data, &params); err != nil {
		t.Fatal(err)
	}
	if request.ServiceID != svcPhoneAudio || request.Data.Cmd != 1 || params.InstanceID != h.instance ||
		params.RequestID != a.requestID || params.Port != 0 || params.Protocol != 1 ||
		params.Freq != 48000 || params.Channels != 2 || params.Encoding != 16 {
		t.Fatalf("unexpected phone-output audio request: %+v %+v", request, params)
	}
	const wireID int64 = 9007199254741001
	offer := rtcDescription{Type: "offer", SDP: "audio-offer"}
	encoded, err := json.Marshal(offer)
	if err != nil {
		t.Fatal(err)
	}
	// The phone uses senderId:1 for service replies, including audio.
	body, err := json.Marshal(map[string]any{
		"senderId": 1, "serviceId": svcPhoneAudio,
		"data": streamCommandData{Ver: "1", Cmd: 2, Data: map[string]any{
			"requestId": a.requestID, "sessionId": wireID, "protocol": 1, "protocolData": string(encoded),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.inbound(control, svcPhoneAudio, body)
	if a.State != "negotiating" || a.wireID != wireID {
		t.Fatalf("audio offer was not accepted: %+v", a)
	}
	found := false
	for len(h.events) != 0 {
		event := (<-h.events).(map[string]any)
		if event["type"] == "offer" {
			found = event["kind"] == "audio" && event["mediaId"] == a.ID && event["description"] == offer
		}
	}
	if !found {
		t.Fatal("audio offer did not reach the native consumer")
	}
	answer := rtcDescription{Type: "answer", SDP: "audio-answer"}
	if _, err := h.command(viewerCommand{Op: "signal", MediaID: a.ID, Description: &answer}); err != nil {
		t.Fatal(err)
	}
	reply := (*sent)[len(*sent)-1]
	var answerData streamPhoneData
	if err := json.Unmarshal(reply.Data.Data, &answerData); err != nil {
		t.Fatal(err)
	}
	var decoded rtcDescription
	if err := json.Unmarshal([]byte(answerData.ProtocolData), &decoded); err != nil {
		t.Fatal(err)
	}
	if reply.ServiceID != svcPhoneAudio || reply.Data.Cmd != 3 || answerData.SessionID != wireID ||
		answerData.Protocol != 1 || answerData.Error != 0 || decoded != answer {
		t.Fatalf("audio answer used the wrong route or payload: %+v %+v", reply, answerData)
	}
	candidateData := `{"type":"candidate","id":"audio","label":0,"candidate":"test-candidate"}`
	receiveStream(t, h, control, svcPhoneAudio, 8, map[string]any{
		"sessionId": wireID, "type": "candidate", "data": candidateData,
	})
	var event map[string]any
	select {
	case received := <-h.events:
		event = received.(map[string]any)
	default:
		t.Fatal("audio ICE did not reach the native consumer")
	}
	candidate, ok := event["candidate"].(rtcCandidate)
	if event["type"] != "candidate" || event["mediaId"] != a.ID || !ok || candidate.Candidate != "test-candidate" {
		t.Fatalf("audio ICE did not reach the native consumer: %+v", event)
	}
	for _, step := range []struct {
		wire  int
		state string
	}{{1, "connected"}, {2, "paused"}, {3, "connected"}, {0, "stopped"}} {
		receiveStream(t, h, control, svcPhoneAudio, 4, map[string]any{"sessionId": wireID, "state": step.wire})
		if a.State != step.state {
			t.Fatalf("audio state %d became %q, want %q", step.wire, a.State, step.state)
		}
	}
	if a.ID != "" || !a.deadline.IsZero() {
		t.Fatalf("stopped audio retained its native media identity or startup deadline: %+v", a)
	}
}

func TestPhoneAudioRejectsMicrophoneWithoutStoppingOutput(t *testing.T) {
	h, control, sent, _ := streamingFixture(t)
	h.startAudioLocked()
	a := h.audio
	a.wireID, a.State = 123456789, "connected"
	receiveStream(t, h, control, svcPhoneAudio, 1, map[string]any{"requestId": 42, "protocol": 1})
	reply := (*sent)[len(*sent)-1]
	var data streamPhoneData
	if err := json.Unmarshal(reply.Data.Data, &data); err != nil {
		t.Fatal(err)
	}
	if reply.ServiceID != svcPhoneAudio || reply.Data.Cmd != 2 || data.RequestID != 42 || data.Error == 0 {
		t.Fatalf("microphone request was not explicitly declined: %+v %+v", reply, data)
	}
	receiveStream(t, h, control, svcPhoneAudio, 2, map[string]any{"requestId": a.requestID + 1, "error": 145})
	receiveStream(t, h, control, svcPhoneAudio, 4, map[string]any{"sessionId": a.wireID + 1, "state": 0})
	if h.audio != a || a.State != "connected" || a.wireID != 123456789 {
		t.Fatalf("microphone or stale session messages interrupted phone output: %+v", h.audio)
	}
}
