package main

/*
#cgo pkg-config: sdl3 libavcodec libavutil libswscale opus
#include "native_decode.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const nativeCandidateLimit = 128

var (
	errNativeWaitingTransport = errors.New("waiting for video transport")
	errNativeWaitingTrack     = errors.New("waiting for a video track")
	errNativeWaitingFrame     = errors.New("waiting for a decoded video frame")
	errNativeTransportFailed  = errors.New("video transport failed")
)

// nativeMedia's maps and all SDL texture operations belong to the UI thread.
// Peer mutexes protect C-owned decode buffers from that thread and RTP workers.
type nativeMedia struct {
	hub    *viewerHub
	peers  map[string]*nativePeer
	early  map[string][]rtcCandidate
	active map[string]bool
	muted  bool
	closed bool
}

type nativePeer struct {
	owner                               *nativeMedia
	id, kind, sessionID                 string
	mu                                  sync.Mutex
	pc                                  *webrtc.PeerConnection
	closed, failed, muted, trackStarted bool
	video                               *C.native_video
	audio                               *C.native_audio
	drawError                           [256]C.char
	done                                chan struct{}
	remote                              chan rtcCandidate
	local                               chan rtcCandidate
	failures                            chan error
	workers                             sync.WaitGroup
}

func newNativeMedia(h *viewerHub) *nativeMedia {
	return &nativeMedia{hub: h, peers: make(map[string]*nativePeer),
		early: make(map[string][]rtcCandidate), active: make(map[string]bool)}
}

func (m *nativeMedia) Handle(event any) {
	if m.closed {
		return
	}
	message, ok := event.(map[string]any)
	if !ok {
		return
	}
	switch message["type"] {
	case "state":
		state, ok := message["state"].(viewerState)
		if !ok {
			return
		}
		active := make(map[string]bool)
		if state.Connected {
			for _, session := range state.Sessions {
				if session.MediaID != "" && session.State != "failed" && session.State != "stopped" {
					active[session.MediaID] = true
				}
			}
			if state.Audio.ID != "" && state.Audio.State != "failed" && state.Audio.State != "stopped" {
				active[state.Audio.ID] = true
			}
		}
		for id, peer := range m.peers {
			if !active[id] {
				peer.close()
				delete(m.peers, id)
			}
		}
		for id := range m.early {
			if !active[id] {
				delete(m.early, id)
			}
		}
		m.active = active
	case "offer":
		id, _ := message["mediaId"].(string)
		kind, _ := message["kind"].(string)
		sessionID, _ := message["sessionId"].(string)
		description, ok := message["description"].(rtcDescription)
		if id == "" || !ok || description.Type != "offer" || description.SDP == "" || (kind != "audio" && kind != "video") {
			m.report(errors.New("native media received an invalid WebRTC offer"))
			return
		}
		if !m.active[id] {
			return
		}
		for oldID, peer := range m.peers {
			if oldID == id || (kind == "audio" && peer.kind == "audio") ||
				(kind == "video" && peer.kind == "video" && peer.sessionID == sessionID) {
				peer.close()
				delete(m.peers, oldID)
			}
		}
		peer := &nativePeer{owner: m, id: id, kind: kind, sessionID: sessionID, muted: m.muted,
			done: make(chan struct{}), remote: make(chan rtcCandidate, nativeCandidateLimit),
			local: make(chan rtcCandidate, nativeCandidateLimit), failures: make(chan error, 1)}
		for _, candidate := range m.early[id] {
			peer.remote <- candidate
		}
		delete(m.early, id)
		m.peers[id] = peer
		peer.workers.Add(1)
		go peer.run(description)
	case "candidate":
		id, _ := message["mediaId"].(string)
		candidate, ok := message["candidate"].(rtcCandidate)
		if !ok || candidate.Candidate == "" || !m.active[id] {
			return
		}
		if peer := m.peers[id]; peer != nil {
			select {
			case peer.remote <- candidate:
			default:
				peer.reject(errors.New("remote ICE candidate queue overflow"))
			}
		} else if len(m.early[id]) < nativeCandidateLimit {
			m.early[id] = append(m.early[id], candidate)
		} else {
			m.report(fmt.Errorf("native media %s: early ICE candidate queue overflow", id))
		}
	}
}

func (m *nativeMedia) report(err error) {
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	m.hub.errorLocked(err)
}

func (m *nativeMedia) Close() {
	if m.closed {
		return
	}
	m.closed = true
	for id, peer := range m.peers {
		peer.close()
		delete(m.peers, id)
	}
	clear(m.early)
	clear(m.active)
}

func (m *nativeMedia) SetMuted(muted bool) {
	m.muted = muted
	for _, peer := range m.peers {
		peer.mu.Lock()
		peer.muted = muted
		if muted && peer.audio != nil {
			var detail [256]C.char
			if C.native_audio_clear(peer.audio, &detail[0], C.size_t(len(detail))) < 0 {
				peer.reject(errors.New(C.GoString(&detail[0])))
			}
		}
		peer.mu.Unlock()
	}
}

// Draw reports readiness honestly: nil means a decoded frame was rendered.
func (m *nativeMedia) Draw(mediaID string, renderer unsafe.Pointer, x, y, w, h float32) error {
	return m.draw(mediaID, renderer, x, y, w, h, 0)
}

// DrawRotated renders a decoded frame at a right angle without touching the
// decoder's output or the phone's declared orientation.
func (m *nativeMedia) DrawRotated(mediaID string, renderer unsafe.Pointer, x, y, w, h float32, angle int) error {
	return m.draw(mediaID, renderer, x, y, w, h, angle)
}

func (m *nativeMedia) draw(mediaID string, renderer unsafe.Pointer, x, y, w, h float32, angle int) error {
	peer := m.peers[mediaID]
	if peer == nil {
		return errNativeWaitingTransport
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.failed {
		return errNativeTransportFailed
	}
	if peer.video == nil {
		return errNativeWaitingTrack
	}
	result := C.native_video_draw_rotated(peer.video, renderer, C.float(x), C.float(y), C.float(w), C.float(h),
		C.int(angle), &peer.drawError[0], C.size_t(len(peer.drawError)))
	if result > 0 {
		return errNativeWaitingFrame
	}
	if result < 0 {
		return errors.New(C.GoString(&peer.drawError[0]))
	}
	return nil
}

// Dimensions returns the decoded frame's current pixel size, or false while no
// frame has arrived. It is used only to choose a window shape and rotation.
func (m *nativeMedia) Dimensions(mediaID string) (int, int, bool) {
	peer := m.peers[mediaID]
	if peer == nil {
		return 0, 0, false
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.video == nil {
		return 0, 0, false
	}
	var width, height, pitch C.int
	var rgba [4]C.uint8_t
	if C.native_video_inspect(peer.video, &width, &height, &pitch, &rgba[0]) == 0 {
		return 0, 0, false
	}
	return int(width), int(height), true
}

// Only codecs actually decoded below are advertised; the phone's SDP is never rewritten.
func newNativePeerConnection(kind, interfaceName string) (*webrtc.PeerConnection, error) {
	engine := &webrtc.MediaEngine{}
	if kind == "audio" {
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
	} else {
		feedback := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}
		parameters := []webrtc.RTPCodecParameters{
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: feedback}, PayloadType: 96},
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0", RTCPFeedback: feedback}, PayloadType: 98},
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=2", RTCPFeedback: feedback}, PayloadType: 100},
		}
		payload := webrtc.PayloadType(102)
		for _, profile := range []string{"42001f", "42e01f", "4d001f", "64001f"} {
			for _, mode := range []string{"1", "0"} {
				parameters = append(parameters, webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType: webrtc.MimeTypeH264, ClockRate: 90000, RTCPFeedback: feedback,
					SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=" + mode + ";profile-level-id=" + profile}, PayloadType: payload})
				payload += 2
			}
		}
		for _, codec := range parameters {
			if err := engine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
			// RTX permits retransmitted packets to reach Pion's receiver while the
			// default NACK interceptor requests missing packets.
			rtx := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", codec.PayloadType)}, PayloadType: codec.PayloadType + 1}
			if err := engine.RegisterCodec(rtx, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
		}
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		return nil, err
	}
	var settings webrtc.SettingEngine
	if interfaceName != "" {
		settings.SetInterfaceFilter(func(name string) bool { return name == interfaceName })
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	return api.NewPeerConnection(webrtc.Configuration{BundlePolicy: webrtc.BundlePolicyMaxBundle, RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire})
}

func (p *nativePeer) reject(err error) {
	select {
	case p.failures <- err:
	default:
	}
}

func (p *nativePeer) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed || p.failed
}

func (p *nativePeer) signal(command viewerCommand) error {
	p.mu.Lock()
	if p.closed || p.failed {
		p.mu.Unlock()
		return io.EOF
	}
	p.mu.Unlock()
	_, err := p.owner.hub.command(command)
	return err
}

func (p *nativePeer) run(offer rtcDescription) {
	defer p.workers.Done()
	err := p.negotiate(offer)
	if err != nil && !p.isClosed() {
		p.fail(err)
	}
}

func (p *nativePeer) negotiate(offer rtcDescription) error {
	pc, err := newNativePeerConnection(p.kind, p.owner.hub.mediaInterface)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = pc.Close()
		return nil
	}
	p.pc = pc
	p.mu.Unlock()
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		select {
		case <-p.done:
		case p.local <- rtcCandidate{Candidate: value.Candidate, SDPMid: value.SDPMid, SDPMLineIndex: value.SDPMLineIndex}:
		default:
			p.reject(errors.New("local ICE candidate queue overflow"))
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed {
			p.reject(errors.New("direct WebRTC transport failed; no automatic reconnect is attempted"))
		}
	})
	pc.OnTrack(p.receiveTrack)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return err
	}
	supported := false
	for _, transceiver := range pc.GetTransceivers() {
		receiver := transceiver.Receiver()
		if receiver == nil {
			continue
		}
		for _, codec := range receiver.GetParameters().Codecs {
			if _, video, err := nativeTrackFormat(codec.MimeType); err == nil &&
				((p.kind == "video") == (video != 0)) {
				supported = true
			}
		}
	}
	if !supported {
		return fmt.Errorf("phone's %s offer has no supported decoder codec", p.kind)
	}
	// Drain ICE received before/during setRemoteDescription before answering.
	for {
		select {
		case candidate := <-p.remote:
			if err := pc.AddICECandidate(nativeICE(candidate)); err != nil {
				return err
			}
		default:
			goto answer
		}
	}
answer:
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}
	local := pc.LocalDescription()
	if err := p.signal(viewerCommand{Op: "signal", MediaID: p.id,
		Description: &rtcDescription{Type: "answer", SDP: local.SDP}}); err != nil {
		return err
	}
	// This loop is the only sender of local ICE: the answer is accepted first.
	for {
		select {
		case <-p.done:
			return nil
		case err := <-p.failures:
			return err
		case candidate := <-p.remote:
			if err := pc.AddICECandidate(nativeICE(candidate)); err != nil {
				return err
			}
		case candidate := <-p.local:
			if err := p.signal(viewerCommand{Op: "signal", MediaID: p.id, Candidate: &candidate}); err != nil {
				return err
			}
		}
	}
}

func nativeICE(candidate rtcCandidate) webrtc.ICECandidateInit {
	return webrtc.ICECandidateInit{Candidate: candidate.Candidate, SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex}
}

func (p *nativePeer) fail(err error) {
	p.mu.Lock()
	if p.closed || p.failed {
		p.mu.Unlock()
		return
	}
	p.failed = true
	pc := p.pc
	p.mu.Unlock()
	p.owner.report(fmt.Errorf("native %s %s: %w", p.kind, p.id, err))
	if pc != nil {
		_ = pc.Close()
	}
	// Stop authenticated remote resources as the browser did; failures remain visible.
	command := viewerCommand{Op: "stop", SessionID: p.sessionID}
	if p.kind == "audio" {
		command = viewerCommand{Op: "stop-audio"}
	}
	if _, stopErr := p.owner.hub.command(command); stopErr != nil && !p.isClosedByOwner() {
		p.owner.report(fmt.Errorf("native media cleanup: %w", stopErr))
	}
}

func (p *nativePeer) isClosedByOwner() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Called only on the SDL thread, after invalidating callbacks and unblocking reads.
func (p *nativePeer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)
	pc := p.pc
	p.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
	p.workers.Wait()
	C.native_video_free(p.video)
	C.native_audio_free(p.audio)
	p.video, p.audio = nil, nil
}

func (p *nativePeer) receiveTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	p.mu.Lock()
	if p.closed || p.failed {
		p.mu.Unlock()
		return
	}
	if p.trackStarted {
		p.mu.Unlock()
		p.reject(errors.New("multiple media tracks in one phone offer are unsupported"))
		return
	}
	p.trackStarted = true
	p.workers.Add(2)
	p.mu.Unlock()
	go func() {
		defer p.workers.Done()
		p.readTrack(track)
	}()
	go func() {
		defer p.workers.Done()
		for {
			if _, _, err := receiver.ReadRTCP(); err != nil {
				return
			}
		}
	}()
}

// The snapshot inspects C buffers under the same lock as Draw, without copying frames.
func (p *nativePeer) decodedSnapshot() (width, height, pitch int, frames uint64, rgba [4]byte, audioSamples uint64, peak, queued int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.video != nil {
		var w, h, stride C.int
		var pixel [4]C.uint8_t
		frames = uint64(C.native_video_inspect(p.video, &w, &h, &stride, &pixel[0]))
		width, height, pitch = int(w), int(h), int(stride)
		for i := range rgba {
			rgba[i] = byte(pixel[i])
		}
	}
	if p.audio != nil {
		var pmax, backlog C.int
		audioSamples = uint64(C.native_audio_inspect(p.audio, &pmax, &backlog))
		peak, queued = int(pmax), int(backlog)
	}
	return
}

func nativeTrackFormat(mime string) (rtp.Depacketizer, int, error) {
	switch strings.ToLower(mime) {
	case "video/h264":
		return &nativeH264Depacketizer{}, 1, nil
	case "video/vp8":
		return &codecs.VP8Packet{}, 2, nil
	case "video/vp9":
		return &codecs.VP9Packet{}, 3, nil
	case "audio/opus":
		return &codecs.OpusPacket{}, 0, nil
	default:
		return nil, 0, fmt.Errorf("unsupported negotiated codec %q", mime)
	}
}

// Pion retains FU-A fragments until an end fragment. A lost end fragment must
// not be prepended to the next NAL or grow an unbounded assembly across frames.
type nativeH264Depacketizer struct {
	codecs.H264Packet
	pending int
}

func (d *nativeH264Depacketizer) Unmarshal(payload []byte) ([]byte, error) {
	fragmented := len(payload) >= 2 && payload[0]&0x1f == 28
	if !fragmented || payload[1]&0x80 != 0 {
		d.H264Packet = codecs.H264Packet{}
		d.pending = 0
	}
	if fragmented {
		d.pending += len(payload) - 2
		if d.pending > 8*1024*1024 {
			d.H264Packet = codecs.H264Packet{}
			d.pending = 0
			return nil, errors.New("H264 fragment assembly exceeds 8 MiB")
		}
	}
	data, err := d.H264Packet.Unmarshal(payload)
	if fragmented && payload[1]&0x40 != 0 {
		d.pending = 0
	}
	return data, err
}

func (p *nativePeer) readTrack(track *webrtc.TrackRemote) {
	codec := track.Codec()
	depacketizer, videoCodec, err := nativeTrackFormat(codec.MimeType)
	if err != nil {
		p.reject(err)
		return
	}
	if (p.kind == "video") != (videoCodec != 0) {
		p.reject(fmt.Errorf("unexpected %s track in %s offer", codec.MimeType, p.kind))
		return
	}
	if (videoCodec == 0 && (codec.ClockRate != 48000 || codec.Channels > 2)) ||
		(videoCodec != 0 && codec.ClockRate != 90000) {
		p.reject(fmt.Errorf("unsupported clock rate/channel count for %s", codec.MimeType))
		return
	}
	p.mu.Lock()
	if p.closed || p.failed {
		p.mu.Unlock()
		return
	}
	var detail [256]C.char
	if videoCodec != 0 {
		p.video = C.native_video_new(C.int(videoCodec), &detail[0], C.size_t(len(detail)))
		if p.video == nil {
			err = errors.New(C.GoString(&detail[0]))
		}
	} else {
		p.audio = C.native_audio_new(&detail[0], C.size_t(len(detail)))
		if p.audio == nil {
			err = errors.New(C.GoString(&detail[0]))
		}
	}
	p.mu.Unlock()
	if err != nil {
		p.reject(err)
		return
	}
	log.Printf("[stream] native %s %s: receiving %s track, clock=%d channels=%d",
		p.kind, p.id, codec.MimeType, codec.ClockRate, codec.Channels)
	builder := samplebuilder.New(256, depacketizer, codec.ClockRate, samplebuilder.WithMaxTimeDelay(200*time.Millisecond))
	var lastPLI, lastDecodeWarning time.Time
	var decodedLogged bool
	var decodedWidth, decodedHeight, decodedPitch, decodedPeak, decodedQueued C.int
	var decodedPixel [4]C.uint8_t
	requestKeyframe := func() {
		if videoCodec == 0 || time.Since(lastPLI) < time.Second {
			return
		}
		lastPLI = time.Now()
		if err := p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil && !p.isClosed() {
			p.reject(fmt.Errorf("requesting video keyframe: %w", err))
		}
	}
	requestKeyframe()
	for {
		packet, _, readErr := track.ReadRTP()
		if readErr != nil {
			if !p.isClosed() {
				p.reject(fmt.Errorf("reading %s RTP: %w", codec.MimeType, readErr))
			}
			return
		}
		// Payload changes must not be decoded with the previous codec's state.
		if !strings.EqualFold(track.Codec().MimeType, codec.MimeType) {
			p.reject(errors.New("phone changed codecs within an active RTP track"))
			return
		}
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			if sample.PrevDroppedPackets != 0 {
				requestKeyframe()
			}
			if len(sample.Data) == 0 {
				continue
			}
			if len(sample.Data) > 8*1024*1024 {
				p.reject(errors.New("encoded media sample exceeds 8 MiB"))
				return
			}
			p.mu.Lock()
			if p.closed || p.failed {
				p.mu.Unlock()
				return
			}
			var result C.int
			if videoCodec != 0 {
				result = C.native_video_push(p.video, (*C.uint8_t)(unsafe.Pointer(&sample.Data[0])), C.int(len(sample.Data)), &detail[0], C.size_t(len(detail)))
			} else {
				muted := C.int(0)
				if p.muted {
					muted = 1
				}
				result = C.native_audio_push(p.audio, (*C.uint8_t)(unsafe.Pointer(&sample.Data[0])), C.int(len(sample.Data)), muted, &detail[0], C.size_t(len(detail)))
			}
			if result >= 0 && !decodedLogged {
				if videoCodec != 0 {
					frames := C.native_video_inspect(p.video, &decodedWidth, &decodedHeight, &decodedPitch, &decodedPixel[0])
					if frames != 0 {
						log.Printf("[stream] native video %s: decoded %s %dx%d pitch=%d frames=%d",
							p.id, codec.MimeType, int(decodedWidth), int(decodedHeight), int(decodedPitch), uint64(frames))
						decodedLogged = true
					}
				} else {
					samples := C.native_audio_inspect(p.audio, &decodedPeak, &decodedQueued)
					if samples != 0 {
						log.Printf("[stream] native audio %s: decoded %s samples=%d PCM-peak=%d queued-bytes=%d muted=%t",
							p.id, codec.MimeType, uint64(samples), int(decodedPeak), int(decodedQueued), p.muted)
						decodedLogged = true
					}
				}
			}
			p.mu.Unlock()
			if result > 0 {
				requestKeyframe()
				if time.Since(lastDecodeWarning) >= time.Second {
					log.Printf("[stream] native %s %s: %s; waiting for a keyframe", p.kind, p.id, C.GoString(&detail[0]))
					lastDecodeWarning = time.Now()
				}
			}
			if result < 0 {
				p.reject(fmt.Errorf("decoding %s: %s", codec.MimeType, C.GoString(&detail[0])))
				return
			}
		}
	}
}
