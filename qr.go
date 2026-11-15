// QR invite generation for the rfproto prototype.
//
// The QR payload is NOT bare JSON: it is the literal ASCII prefix
// "motorolardpconnection" immediately followed by a JSON object. Format and
// field set are wire-confirmed from two real PC sessions — see
// raw/qr_decoded.txt and docs/05-route-a-crypto.md §8.
//
// The phone scans this QR, validates its TLS connection to our wsPort against
// the embedded `fp` (base64 of sha256(cert DER)), then dials that port. Because
// we compute `fp` from the very cert our TLS server presents, the pin matches
// by construction — this is what makes the pairing hand-shake succeed.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// qrPrefix is the mandatory literal prepended to the JSON. The phone strips it
// before unmarshalling.
const qrPrefix = "motorolardpconnection"

// inviteOuter mirrors the top-level QR JSON. Field order matches the observed
// wire order (Go's encoding/json emits struct fields in declaration order).
type inviteOuter struct {
	Version string       `json:"version"`
	WsData  inviteWsData `json:"wsData"`
	Ips     []string     `json:"ips,omitempty"`
}

// inviteWsData mirrors the observed wsData object. Values marked "wire" come
// verbatim from a real Windows PC QR; identity/crypto fields are ours.
type inviteWsData struct {
	Ver        int                   `json:"ver"`      // wire: 2
	Cmd        int                   `json:"cmd"`      // wire: 1
	DeviceId   string                `json:"deviceId"` // ours, base64
	DeviceName string                `json:"deviceName"`
	Fp         string                `json:"fp"`    // ours: base64(sha256(cert DER)) — phone pins this
	Token      string                `json:"token"` // ours: random base64 (per session)
	Policies   []map[string][]string `json:"policies"`
	WsPort     int                   `json:"wsPort"`          // our TLS/WebSocket listen port
	AppVer     string                `json:"appVer"`          // wire: "09.0.3.013.000"
	VerCode    int                   `json:"verCode"`         // wire: 903013000
	Os         int                   `json:"os"`              // wire: 6 (Windows PC)
	ScType     int                   `json:"scType"`          // wire: 2
	Type       int                   `json:"type"`            // wire: 4
	I3         int                   `json:"i3"`              // wire: 1
	ServiceId  int                   `json:"serviceId"`       // wire: 19
	ReqID      int                   `json:"reqId,omitempty"` // correlated service-19 endpoint replies
}

// newInviteWsData assembles the wsData object. It is shared by the QR invite and
// by the BLE negotiation characteristic: the two carry the same WsData type, and
// the phone's own log of the value it received over GATT (see ble.go) has
// exactly this field set — deviceId, fp, token, policies, wsPort, os/type/scType/
// i3, appVer/verCode, serviceId=19.
func newInviteWsData(deviceID, deviceName, fp, token string, lanIPs []string, wfd string, wsPort int) inviteWsData {
	var policies []map[string][]string
	if len(lanIPs) > 0 {
		policies = append(policies, map[string][]string{"wifi": lanIPs})
	}
	if wfd != "" {
		policies = append(policies, map[string][]string{"wfd": {wfd}})
	}
	return inviteWsData{
		Ver:        2,
		Cmd:        1,
		DeviceId:   deviceID,
		DeviceName: deviceName,
		Fp:         fp,
		Token:      token,
		Policies:   policies,
		WsPort:     wsPort,
		AppVer:     "09.0.3.013.000",
		VerCode:    903013000,
		Os:         6,
		ScType:     2,
		Type:       4,
		I3:         1,
		ServiceId:  19,
	}
}

// buildInvite assembles the exact QR string (prefix + JSON).
func buildInvite(deviceID, deviceName, fp, token string, lanIPs []string, wfd string, wsPort int) (string, error) {
	inv := inviteOuter{
		Version: "7.0.0",
		WsData:  newInviteWsData(deviceID, deviceName, fp, token, lanIPs, wfd, wsPort),
		Ips:     lanIPs,
	}
	b, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	return qrPrefix + string(b), nil
}

// wsDataJSON marshals only the wsData object, with no QR wrapper and no
// "motorolardpconnection" prefix. That object is what the real PC exposes on the
// BLE negotiation characteristic (see ble.go and docs/05 §12.3): the phone reads
// it to learn the peer's wsPort/fp/token when the mDNS/QR path has not been used.
func wsDataJSON(ws inviteWsData) ([]byte, error) {
	return json.Marshal(ws)
}

// newToken returns a random base64 token (~43 chars, 32 bytes) matching the
// observed length of the real PC token.
func newToken() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.StdEncoding.EncodeToString(raw)
}

// lanIPList returns the non-loopback IPv4 addresses as a slice (for the QR
// `ips` field and the `wifi` policy).
func lanIPList() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// renderQR writes the payload to a PNG and prints an ASCII version to the
// terminal so the phone can scan it straight off the screen.
func renderQR(payload, pngPath string) {
	if pngPath != "" {
		if err := qrcode.WriteFile(payload, qrcode.Medium, 512, pngPath); err != nil {
			log.Printf("[qr] write %s: %v", pngPath, err)
		} else {
			log.Printf("[qr] wrote %s (512x512 PNG)", pngPath)
		}
	}
	log.Printf("[qr] payload (%d bytes): %s", len(payload), payload)
	printASCIIQR(payload)
}

// printASCIIQR renders the QR to the terminal using Unicode half-blocks, so
// each character cell shows two vertical modules. That keeps the width at one
// column per module (fits a normal terminal — no wrapping) while preserving the
// square module aspect on a ~2:1 terminal font.
func printASCIIQR(payload string) {
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		log.Printf("[qr] render: %v", err)
		return
	}
	bm := qr.Bitmap()
	h, w := len(bm), len(bm[0])
	if h%2 == 1 { // pad to an even number of rows for the 2-rows-per-cell packing
		bm = append(bm, make([]bool, w))
		h++
	}
	var sb strings.Builder
	sb.WriteString("\n")
	for y := 0; y < h; y += 2 {
		sb.WriteString("  ")
		for x := 0; x < w; x++ {
			top, bot := bm[y][x], bm[y+1][x]
			switch {
			case top && bot:
				sb.WriteString("█")
			case top && !bot:
				sb.WriteString("▀")
			case !top && bot:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}
	log.Printf("[qr] %dx%d modules — scan with the Ready For app:", w, h)
	fmt.Fprint(os.Stderr, sb.String())
}
