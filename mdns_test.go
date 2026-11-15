package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Construct frames independently of the production encoder. In particular,
// these fixtures authenticate all 17 header bytes, unlike the older discovery
// cipher description with nil AAD.
func negotiationTestFrame(t *testing.T, key, sender string, plain []byte, headerAAD bool) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(decoded)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	id, err := base64.StdEncoding.DecodeString(sender)
	if err != nil || len(id) != 16 {
		t.Fatal("invalid test sender")
	}
	frame := make([]byte, 29)
	copy(frame, id)
	frame[16] = 1
	copy(frame[17:29], []byte("test-nonce12"))
	var aad []byte
	if headerAAD {
		aad = frame[:17]
	}
	return gcm.Seal(frame, frame[17:29], plain, aad)
}

func TestLANReconnectNegotiation(t *testing.T) {
	setupPairingTest(t)
	previousEndpoint, previousInfo, previousReply, previousCapture := reconnectEndpoint, ourInfo, replyToFirst, captureDir
	t.Cleanup(func() {
		reconnectEndpoint, ourInfo, replyToFirst, captureDir = previousEndpoint, previousInfo, previousReply, previousCapture
	})
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	phoneID := base64.StdEncoding.EncodeToString([]byte("phone-session-id"))
	pcID := base64.StdEncoding.EncodeToString([]byte("pc-identity-1234"))
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "stable-phone", Key: key}); err != nil {
		t.Fatal(err)
	}
	reconnectEndpoint = newInviteWsData(pcID, "Linux", "base64-control-pin", "fresh-restart-token", []string{"192.0.2.1"}, "", 19527)
	ourInfo = replyInfo{DeviceID: pcID, Name: "Linux", SN: "test-sn"}
	replyToFirst, captureDir = "auto", ""
	srv := pairingServer(t)
	dialer := websocket.Dialer{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig}
	c, _, err := dialer.Dial("wss"+strings.TrimPrefix(srv.URL, "https")+"/readyfor/mdns", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var endpoint inviteWsData
	for _, reqID := range []int{7, 8} {
		request, _ := json.Marshal(map[string]any{"ver": 1, "cmd": 0, "deviceId": phoneID, "serviceId": 19, "reqId": reqID, "wsPort": 0})
		frame := negotiationTestFrame(t, key, phoneID, request, true)
		if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatal(err)
		}
		mt, out, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt != websocket.BinaryMessage || len(out) < 45 || out[16] != 1 || base64.StdEncoding.EncodeToString(out[:16]) != pcID {
			t.Fatal("endpoint response is not an encrypted PC-origin binary frame")
		}
		decodedKey, _ := base64.StdEncoding.DecodeString(key)
		block, _ := aes.NewCipher(decodedKey)
		gcm, _ := cipher.NewGCM(block)
		plain, err := gcm.Open(nil, out[17:29], out[29:], out[:17])
		if err != nil {
			t.Fatalf("endpoint response did not authenticate: %v", err)
		}
		if err := json.Unmarshal(plain, &endpoint); err != nil {
			t.Fatal(err)
		}
		if endpoint.Cmd != 1 || endpoint.ReqID != reqID || endpoint.DeviceId != pcID || endpoint.Token != "fresh-restart-token" || endpoint.Fp != "base64-control-pin" || endpoint.WsPort != 19527 || len(endpoint.Policies) != 1 {
			t.Fatalf("incorrect correlated endpoint announcement: %s", plain)
		}
	}
	// After endpoint refresh, the paired manager hello must authenticate with
	// its nested device.key, independent of a previous QR token/session ID.
	control := pairDial(t, srv)
	hello := reconnectHello("stable-phone", key)
	reply := pairExchange(t, control, hello)
	if reply.Data.Paired != 1 || !reply.Data.Device.IsPaired || reply.Data.Device.Key != key {
		t.Fatal("negotiated endpoint could not reconnect a saved phone")
	}
}

func TestLANRejectsUnauthenticatedNegotiation(t *testing.T) {
	setupPairingTest(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	wrongKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32))
	phoneID := base64.StdEncoding.EncodeToString([]byte("phone-session-id"))
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "stable-phone", Key: key}); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"cmd": 0, "deviceId": phoneID, "serviceId": 19, "reqId": 1})
	valid := negotiationTestFrame(t, key, phoneID, request, true)
	tampered := append([]byte(nil), valid...)
	tampered[0] ^= 1
	plaintext := append(append([]byte(nil), valid[:17]...), request...)
	plaintext[16] = 2
	srv := pairingServer(t)
	for _, tc := range []struct {
		name  string
		mt    int
		frame []byte
	}{
		{"plaintext downgrade", websocket.BinaryMessage, plaintext},
		{"unknown pairing key", websocket.BinaryMessage, negotiationTestFrame(t, wrongKey, phoneID, request, true)},
		{"unauthenticated header", websocket.BinaryMessage, negotiationTestFrame(t, key, phoneID, request, false)},
		{"tampered sender identity", websocket.BinaryMessage, tampered},
		{"text frame", websocket.TextMessage, valid},
		{"truncated frame", websocket.BinaryMessage, valid[:20]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialer := websocket.Dialer{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig}
			c, _, err := dialer.Dial("wss"+strings.TrimPrefix(srv.URL, "https")+"/readyfor/mdns", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err := c.WriteMessage(tc.mt, tc.frame); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.ReadMessage(); !websocket.IsCloseError(err, 3123) {
				t.Fatalf("expected authenticated rejection without an endpoint/unpair frame, got %v", err)
			}
		})
	}
	if p, ok := trustedAdults.lookup("stable-phone"); !ok || p.Key != key {
		t.Fatal("invalid negotiation erased local pairing")
	}
}
