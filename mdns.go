package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// The discovery WebSocket and the control WebSocket have different protocols.
// /readyfor/mdns carries binary service-19 negotiation frames, authenticated
// with the saved phone key. Its certificate pin in mDNS is hex; the control
// endpoint announcement inside the frame carries a base64 pin.
// Protocol-only research and references: docs/INVESTIGATION.md, reconnect notes.
var reconnectEndpoint inviteWsData

func openNegotiation(frame []byte, store *peerStore) ([]byte, cipher.AEAD, string, error) {
	if store == nil || len(frame) < 45 || frame[16] != 1 {
		return nil, nil, "", errors.New("expected encrypted paired negotiation")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	// The phone's current discovery ID can rotate between sessions. Verify
	// against saved keys rather than trusting that unauthenticated header ID
	// as a stable identity. Successful GCM authentication identifies the peer.
	for _, p := range store.peers {
		key, err := base64.StdEncoding.DecodeString(p.Key)
		if err != nil {
			continue
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			continue
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, nil, "", err
		}
		plain, err := aead.Open(nil, frame[17:29], frame[29:], frame[:17])
		if err == nil {
			return plain, aead, p.RealDeviceID, nil
		}
	}
	return nil, nil, "", errors.New("negotiation authentication failed")
}

func sealNegotiation(aead cipher.AEAD, id string, plain []byte) ([]byte, error) {
	sender, err := base64.StdEncoding.Strict().DecodeString(id)
	if err != nil || len(sender) != 16 {
		return nil, errors.New("negotiation requires a 16-byte PC identity")
	}
	out := make([]byte, 29, len(plain)+45)
	copy(out[:16], sender)
	out[16] = 1
	if _, err := rand.Read(out[17:29]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[17:29], plain, out[:17]), nil
}

// endpointReply is shared by the LAN and BLE transports. The decrypted
// service-19 request is authenticated before any endpoint/token is disclosed.
func endpointReply(frame, plain []byte, aead cipher.AEAD) ([]byte, int, error) {
	var req struct {
		Cmd       *int   `json:"cmd"`
		DeviceID  string `json:"deviceId"`
		ReqID     int    `json:"reqId"`
		ServiceID int    `json:"serviceId"`
	}
	if json.Unmarshal(plain, &req) != nil || req.Cmd == nil || *req.Cmd != 0 || req.ServiceID != 19 ||
		req.DeviceID != base64.StdEncoding.EncodeToString(frame[:16]) {
		return nil, 0, errors.New("invalid negotiation request")
	}
	reply := reconnectEndpoint
	if reply.Fp == "" || reply.Token == "" || reply.WsPort <= 0 || len(reply.Policies) == 0 {
		return nil, 0, errors.New("endpoint is not configured")
	}
	reply.Ver, reply.Cmd, reply.ReqID = 3, 1, req.ReqID
	plain, err := json.Marshal(reply)
	if err != nil {
		return nil, 0, err
	}
	out, err := sealNegotiation(aead, reply.DeviceId, plain)
	return out, req.ReqID, err
}

func handleMDNS(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[mdns-ws] upgrade: %v", err)
		return
	}
	defer c.Close()
	c.SetReadLimit(64 << 10)
	var aead cipher.AEAD
	peerID := ""
	for {
		_ = c.SetReadDeadline(time.Now().Add(time.Minute))
		mt, frame, err := c.ReadMessage()
		if err != nil {
			return
		}
		var plain []byte
		if mt != websocket.BinaryMessage || len(frame) < 45 || frame[16] != 1 {
			err = errors.New("expected encrypted binary negotiation")
		} else if aead == nil {
			plain, aead, peerID, err = openNegotiation(frame, trustedAdults)
		} else {
			plain, err = aead.Open(nil, frame[17:29], frame[29:], frame[:17])
		}
		if err != nil {
			log.Printf("[mdns-ws] %s rejected: %v", r.RemoteAddr, err)
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(3123, "verify failure"), time.Now().Add(time.Second))
			return
		}
		out, reqID, err := endpointReply(frame, plain, aead)
		if err != nil {
			log.Printf("[mdns-ws] endpoint reply: %v", err)
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteMessage(websocket.BinaryMessage, out); err != nil {
			return
		}
		log.Printf("[mdns-ws] authenticated phone=%s reqId=%d; announced control port=%d", peerID, reqID, reconnectEndpoint.WsPort)
	}
}
