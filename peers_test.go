package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func setupPairingTest(t *testing.T) {
	t.Helper()
	previous, token := trustedAdults, qrToken
	store, err := openPeerStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trustedAdults = store
	var hello authHello
	if err := json.Unmarshal([]byte(capturedHello), &hello); err != nil {
		t.Fatal(err)
	}
	qrToken = hello.Data.Token
	t.Cleanup(func() {
		store.close()
		trustedAdults, qrToken = previous, token
	})
}

// Wait for upgraded handlers too: httptest.Server.Close only waits for HTTP.
func pairingServer(t *testing.T) *httptest.Server {
	t.Helper()
	var handlers sync.WaitGroup
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handle(w, r)
	}))
	t.Cleanup(func() {
		srv.Close()
		handlers.Wait()
	})
	return srv
}

func pairDial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	// httptest's TLS certificate is trusted via its own client, never by a
	// production InsecureSkipVerify setting.
	dialer := websocket.Dialer{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig}
	c, _, err := dialer.Dial("wss"+strings.TrimPrefix(srv.URL, "https")+"/readyfor/", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	return c
}

func pairExchange(t *testing.T, c *websocket.Conn, msg string) pcDiscoveryMsg {
	t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	var reply pcDiscoveryMsg
	if err := c.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func reconnectHello(id, key string) string {
	b, _ := json.Marshal(map[string]any{
		"serviceId": 0, "srcServiceId": 0,
		"data": map[string]any{
			"cmd": 0, "paired": 1,
			"device": map[string]string{"deviceId": "new-session-id", "realDeviceId": id, "key": key},
		},
	})
	return string(b)
}

func TestDurablePairing(t *testing.T) {
	setupPairingTest(t)
	oldMode, oldAck, oldCmd, oldInfo, oldCapture := replyToFirst, pairAckMode, pairAckCmd, ourInfo, captureDir
	t.Cleanup(func() {
		replyToFirst, pairAckMode, pairAckCmd, ourInfo, captureDir = oldMode, oldAck, oldCmd, oldInfo, oldCapture
	})
	replyToFirst, pairAckMode, pairAckCmd, captureDir = "auto", "auto", 1, ""
	ourInfo = replyInfo{DeviceID: "linux-pc", Name: "Linux", SN: "test-serial"}
	url := pairingServer(t)
	c := pairDial(t, url)
	first := pairExchange(t, c, capturedHello)
	if first.Data.Paired != 0 || first.Data.Device.IsPaired || first.Data.Device.Key != "" {
		t.Fatal("new phone incorrectly marked trusted before confirmation")
	}
	ack := pairExchange(t, c, capturedPairConfirm)
	key := ack.Data.Device.Key
	id := "Icca7GgDY7+U16HiYxvNrg=="
	if ack.Data.Cmd != 1 || !ack.Data.Device.IsPaired || !validPairKey(key) {
		t.Fatal("pair-confirm did not establish shared credentials")
	}
	// Observe disk at acknowledgment, not merely after handler shutdown.
	disk, err := readPeers(trustedAdults.dir)
	if err != nil || disk[id].Key != key {
		t.Fatalf("acknowledged before persistence: %v", err)
	}
	info, err := os.Stat(filepath.Join(trustedAdults.dir, "peers.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe trust file permissions: %v", err)
	}
	c.Close()

	// Load a separate store instance as a restarted daemon would.
	dir := trustedAdults.dir
	if err := trustedAdults.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openPeerStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	trustedAdults = reopened
	t.Cleanup(func() { reopened.close() })
	qrToken = "a-different-invite-after-restart"
	r := pairDial(t, url)
	reply := pairExchange(t, r, reconnectHello(id, key))
	if reply.Data.Paired != 1 || !reply.Data.Device.IsPaired || reply.Data.Device.Key != key {
		t.Fatal("reconnect lost persisted trust")
	}
}

func TestRejectUntrustedConnections(t *testing.T) {
	setupPairingTest(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "known-phone", Key: key}); err != nil {
		t.Fatal(err)
	}
	url := pairingServer(t)
	for _, tc := range []struct{ name, msg string }{
		{"wrong QR", strings.Replace(capturedHello, qrToken, "wrong-token", 1)},
		{"wrong reconnect key", reconnectHello("known-phone", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))},
		{"key in wrong location", `{"serviceId":0,"srcServiceId":0,"data":{"cmd":0,"paired":1,"key":"` + key + `","device":{"deviceId":"new-session-id","realDeviceId":"known-phone"}}}`},
		{"unknown phone", reconnectHello("unknown-phone", key)},
		{"features before hello", `{"serviceId":25,"srcServiceId":25,"data":{"cmd":1}}`},
		{"missing envelope fields", `{"data":{"cmd":0,"device":{"deviceId":"phone"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := pairDial(t, url)
			if err := c.WriteMessage(websocket.TextMessage, []byte(tc.msg)); err != nil {
				t.Fatal(err)
			}
			_, _, err := c.ReadMessage()
			if !websocket.IsCloseError(err, 3123) {
				t.Fatalf("expected verification rejection, got %v", err)
			}
		})
	}
}

func TestPairConfirmIdentityAndKey(t *testing.T) {
	setupPairingTest(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for _, tc := range []struct{ name, id, key string }{
		{"identity changed", "other-phone", key},
		{"empty key", "phone", ""},
		{"short key", "phone", "YWJj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := peerAuth{realID: "phone"}
			msg, _ := json.Marshal(map[string]any{"data": map[string]any{"device": map[string]string{"realDeviceId": tc.id, "key": tc.key}}})
			if _, err := a.confirm(msg, trustedAdults); err == nil {
				t.Fatal("accepted invalid pair-confirm")
			}
			if _, ok := trustedAdults.lookup(tc.id); ok {
				t.Fatal("invalid confirmation was persisted")
			}
		})
	}
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "phone", Key: key}); err != nil {
		t.Fatal(err)
	}
	newKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "phone", Key: newKey}); err == nil {
		t.Fatal("silently replaced existing pairing key")
	}
	p, _ := trustedAdults.lookup("phone")
	if p.Key != key {
		t.Fatal("failed replacement changed persisted trust")
	}
}

func TestPeerStoreFailureAndCommands(t *testing.T) {
	dir := t.TempDir()
	s, err := openPeerStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if other, err := openPeerStore(dir); err == nil {
		other.close()
		t.Fatal("allowed competing trust store writers")
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	p := pairedPhone{RealDeviceID: "phone", Name: "Test phone", Key: key}
	if err := s.remember(p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := peersCommand([]string{"-dir", dir, "list"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), key) || strings.Contains(out.String(), `"key"`) || !strings.Contains(out.String(), "Test phone") {
		t.Fatalf("unsafe or incomplete listing: %s", out.String())
	}
	if err := peersCommand([]string{"-dir", dir, "forget", "phone"}, &out); err == nil {
		t.Fatal("forgot a phone while its server was active")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := peersCommand([]string{"-dir", dir, "forget", "phone"}, &out); err != nil {
		t.Fatal(err)
	}
	peers, err := readPeers(dir)
	if err != nil || len(peers) != 0 {
		t.Fatalf("forget did not persist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peers.json"), []byte("broken JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bad, err := openPeerStore(dir); err == nil {
		bad.close()
		t.Fatal("silently reset corrupt trust state")
	}
}

func TestPairingPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := openPeerStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	// A directory at the destination makes the atomic rename fail, even as root.
	if err := os.Mkdir(filepath.Join(dir, "peers.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := peerAuth{}
	if _, err := a.confirm([]byte(capturedPairConfirm), s); err == nil {
		t.Fatal("accepted pairing without durable storage")
	}
	if a.trusted {
		t.Fatal("persistence failure authenticated the session")
	}
}
