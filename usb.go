package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
)

const usbDiscoveryService = "_readyfor._tcp"

type usbLink struct {
	iface net.Interface
	addr  string
	peer  net.IP
}

// Resolve an already configured link. rfproto doesn't change Android's USB
// mode or the host's routes.
func openUSBLink(name, phone string) (usbLink, error) {
	peer := net.ParseIP(phone).To4()
	if peer == nil || !peer.IsGlobalUnicast() {
		return usbLink{}, fmt.Errorf("-usb-peer must be the phone's IPv4 address on the USB link")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return usbLink{}, fmt.Errorf("USB Ethernet interface %q: %w", name, err)
	}
	subsystem, err := os.Readlink(filepath.Join("/sys/class/net", name, "device/subsystem"))
	if err != nil || filepath.Base(subsystem) != "usb" {
		return usbLink{}, fmt.Errorf("interface %q is not backed by a USB device", name)
	}
	if iface.Flags&net.FlagUp == 0 {
		return usbLink{}, fmt.Errorf("USB Ethernet interface %q is down", name)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return usbLink{}, fmt.Errorf("addresses for USB Ethernet interface %q: %w", name, err)
	}
	var link *net.IPNet
	for _, candidate := range addrs {
		subnet, ok := candidate.(*net.IPNet)
		if !ok || subnet.IP.To4() == nil || !subnet.Contains(peer) || subnet.IP.Equal(peer) {
			continue
		}
		if link != nil {
			return usbLink{}, fmt.Errorf("multiple IPv4 addresses on %q reach USB peer %s", name, peer)
		}
		link = subnet
	}
	if link == nil {
		return usbLink{}, fmt.Errorf("no IPv4 address on %q shares a subnet with USB peer %s; check USB mode and DHCP", name, peer)
	}
	return usbLink{iface: *iface, addr: link.IP.String(), peer: peer}, nil
}

func (link usbLink) restrict(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !link.peer.Equal(net.ParseIP(host)) {
			http.Error(w, "not the configured USB peer", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The phone opens this socket using the certificate pin in the USB mDNS
// announcement, then uses the returned endpoint for the normal /readyfor/
// handshake. Service 1 announces presence; it is not proof of a paired identity.
func usbDiscoveryHandler(endpoint inviteWsData) http.HandlerFunc {
	slot := make(chan struct{}, 1)
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			http.Error(w, "USB discovery already connected", http.StatusConflict)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadLimit(16 << 10)
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		var hello struct {
			Action    int    `json:"action"`
			ServiceID int    `json:"serviceId"`
			Status    int    `json:"status"`
			DeviceID  string `json:"deviceId"`
			Device    struct {
				ID string `json:"deviceId"`
			} `json:"device"`
		}
		if c.ReadJSON(&hello) != nil || hello.Action != 1 || hello.ServiceID != 1 || hello.Status != 1 ||
			hello.DeviceID == "" || hello.Device.ID != hello.DeviceID {
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if c.WriteJSON(endpoint) != nil {
			return
		}

		// Keep discovery alive independently of the authenticated control socket.
		// The phone sends status messages here, not pairing or media frames.
		extend := func(string) error { return c.SetReadDeadline(time.Now().Add(60 * time.Second)) }
		c.SetPongHandler(extend)
		_ = extend("")
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
						_ = c.Close()
						return
					}
				}
			}
		}()
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}
}
