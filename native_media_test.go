package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

func nativeFixture(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile("testdata/native-" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	data, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeH264FragmentAssembly(t *testing.T) {
	// A fragmented IDR must emerge as Annex B, not as FU-A RTP payload bytes.
	annexB := append([]byte{0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0x31}, 160)...)
	payloads := (&codecs.H264Payloader{}).Payload(24, annexB)
	if len(payloads) < 2 {
		t.Fatal("test did not fragment H264")
	}
	depacketizer, _, err := nativeTrackFormat("video/H264")
	if err != nil {
		t.Fatal(err)
	}
	builder := samplebuilder.New(32, depacketizer, 90000)
	for i, payload := range payloads {
		builder.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: 9000, Marker: i == len(payloads)-1}, Payload: payload})
	}
	builder.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(len(payloads)), Timestamp: 12000, Marker: true}, Payload: []byte{0x65, 1}})
	sample := builder.Pop()
	if sample == nil || !bytes.Equal(sample.Data, annexB) {
		t.Fatalf("fragmented H264 was not reassembled: %#v", sample)
	}
	d := &nativeH264Depacketizer{}
	if _, err := d.Unmarshal([]byte{0x7c, 0x85, 0xaa}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Unmarshal([]byte{0x7c, 0x85, 0xbb}); err != nil {
		t.Fatal(err)
	}
	recovered, err := d.Unmarshal([]byte{0x7c, 0x45, 0xcc})
	if err != nil || !bytes.Equal(recovered, []byte{0, 0, 0, 1, 0x65, 0xbb, 0xcc}) {
		t.Fatalf("lost FU-A end poisoned next NAL: %x, %v", recovered, err)
	}
	if _, _, err := nativeTrackFormat("video/AV1"); err == nil {
		t.Fatal("unsupported codec was silently accepted")
	}
}

func TestNativePionDecode(t *testing.T) {
	for _, name := range []string{"vp8", "h264", "opus"} {
		t.Run(name, func(t *testing.T) {
			nativeDecodeTest(t, name, 16, 16)
		})
	}
}

func TestNativeVP8PortraitReopen(t *testing.T) {
	// 360px requires a padded RGBA row: swscale's SIMD converter writes
	// 368px. A fresh decoder after close must remain safe to decode again.
	for range 3 {
		t.Run("open", func(t *testing.T) {
			nativeDecodeTest(t, "vp8-portrait", 360, 800)
		})
	}
}

func nativeDecodeTest(t *testing.T, name string, wantWidth, wantHeight int) {
	t.Helper()
	kind := "video"
	payload := nativeFixture(t, name)
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	var payloader rtp.Payloader = &codecs.VP8Payloader{}
	samplesPerFrame := uint32(3000)
	if name == "h264" {
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}
		payloader = &codecs.H264Payloader{}
	}
	if name == "opus" {
		kind = "audio"
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		payloader = &codecs.OpusPayloader{}
		samplesPerFrame = 960
	}
	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer offerer.Close()
	answerer, err := newNativePeerConnection(kind, "")
	if err != nil {
		t.Fatal(err)
	}
	peer := &nativePeer{kind: kind, id: "loopback", pc: answerer, muted: true,
		done: make(chan struct{}), failures: make(chan error, 1)}
	defer peer.close()
	answerer.OnTrack(peer.receiveTrack)
	track, err := webrtc.NewTrackLocalStaticRTP(capability, "phone", "native-loopback")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := offerer.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	nativeNegotiateTest(t, offerer, answerer)
	packetizer := rtp.NewPacketizer(1200, 96, 1234, payloader, rtp.NewFixedSequencer(1), capability.ClockRate)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-peer.failures:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatal("Pion RTP did not produce decoded native media")
		case <-tick.C:
			for _, packet := range packetizer.Packetize(payload, samplesPerFrame) {
				if err := track.WriteRTP(packet); err != nil {
					t.Fatal(err)
				}
			}
			width, height, pitch, frames, rgba, decoded, peak, queued := peer.decodedSnapshot()
			if kind == "video" && frames > 0 {
				if pitch < width*4 || pitch%64 != 0 {
					t.Fatalf("decoded frame lacks SIMD row padding: width=%d pitch=%d", width, pitch)
				}
				if width != wantWidth || height != wantHeight || rgba[3] != 255 {
					t.Fatalf("unexpected decoded frame: %dx%d RGBA=%v", width, height, rgba)
				}
				if name != "vp8-portrait" && (rgba[0] < 200 || rgba[1] > 40 || rgba[2] > 40) {
					t.Fatalf("unexpected decoded red frame: %dx%d RGBA=%v", width, height, rgba)
				}
				peer.close()
				if peer.video != nil {
					t.Fatal("video decoder survived close")
				}
				return
			}
			if kind == "audio" && decoded >= 960 {
				if peak == 0 || queued != 0 {
					t.Fatalf("Opus decode/mute failed: peak=%d queued=%d", peak, queued)
				}
				peer.close()
				if peer.audio != nil {
					t.Fatal("audio decoder survived close")
				}
				return
			}
		}
	}
}

func nativeNegotiateTest(t *testing.T, offerer, answerer *webrtc.PeerConnection) {
	t.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("offer ICE gathering timed out")
	}
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered = webrtc.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("answer ICE gathering timed out")
	}
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMediaPrunesAuthoritativeState(t *testing.T) {
	hub := newViewerHub()
	defer hub.close()
	media := newNativeMedia(hub)
	defer media.Close()
	media.Handle(map[string]any{"type": "state", "state": viewerState{Connected: true,
		Sessions: []viewerSession{{ID: "s1", MediaID: "v1", State: "negotiating"}},
		Audio:    viewerAudio{ID: "a1", State: "negotiating"}}})
	media.Handle(map[string]any{"type": "candidate", "mediaId": "v1", "candidate": rtcCandidate{Candidate: "candidate:queued"}})
	media.Handle(map[string]any{"type": "candidate", "mediaId": "a1", "candidate": rtcCandidate{Candidate: "candidate:queued"}})
	if len(media.early) != 2 {
		t.Fatalf("early candidates lost: %v", media.early)
	}
	peer := &nativePeer{id: "a1", kind: "audio", done: make(chan struct{})}
	media.peers["a1"] = peer
	media.Handle(map[string]any{"type": "state", "state": viewerState{Connected: true, Audio: viewerAudio{ID: "a1", State: "stopped"}}})
	if len(media.peers) != 0 || len(media.early) != 0 || !peer.closed {
		t.Fatal("obsolete media survived authoritative stopped state")
	}
}
