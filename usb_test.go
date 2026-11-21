package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestUSBDiscoveryInvitation(t *testing.T) {
	endpoint := newInviteWsData("test-pc", "PC", "test-pin", "test-invite", nil, "", 19527)
	endpoint.Policies = []map[string][]string{{"usb": {"192.0.2.1"}}}
	for _, tc := range []struct {
		name, peer, hello string
		wantStatus        int
		wantInvite        bool
	}{
		{"other source", "192.0.2.42", `{"action":1,"serviceId":1,"status":1,"deviceId":"phone","device":{"deviceId":"phone"}}`, http.StatusForbidden, false},
		{"mismatched identity", "127.0.0.1", `{"action":1,"serviceId":1,"status":1,"deviceId":"phone","device":{"deviceId":"other"}}`, 0, false},
		{"cable hello", "127.0.0.1", `{"action":1,"serviceId":1,"status":1,"deviceId":"phone","device":{"deviceId":"phone"}}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := usbLink{peer: net.ParseIP(tc.peer)}
			srv := httptest.NewServer(link.restrict(usbDiscoveryHandler(endpoint)))
			defer srv.Close()
			c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if tc.wantStatus != 0 {
				if err == nil {
					c.Close()
					t.Fatal("upgraded a connection from the wrong peer")
				}
				if resp == nil || resp.StatusCode != tc.wantStatus {
					t.Fatalf("expected HTTP %d, got %v (%v)", tc.wantStatus, resp, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.WriteMessage(websocket.TextMessage, []byte(tc.hello)); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var reply inviteWsData
			err = c.ReadJSON(&reply)
			if !tc.wantInvite {
				if _, closed := err.(*websocket.CloseError); !closed {
					t.Fatalf("expected the invalid hello to close the connection, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if reply.ServiceId != 19 || reply.Cmd != 1 || len(reply.Policies) != 1 || len(reply.Policies[0]["usb"]) != 1 || reply.Policies[0]["usb"][0] != "192.0.2.1" {
				t.Fatalf("unexpected cable negotiation reply: %+v", reply)
			}
		})
	}
}
