package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// Exercise the actual read-only D-Bus property and emitted signal: using the
// remote Properties.Set method, or EmitFalse, loses the phone's endpoint reply.
// A private bus keeps this independent of the system bus and Bluetooth adapter.
func TestBLEEncryptedEndpointDelivery(t *testing.T) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon unavailable")
	}
	setupPairingTest(t)
	previous := reconnectEndpoint
	t.Cleanup(func() { reconnectEndpoint = previous })
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	phoneID := base64.StdEncoding.EncodeToString([]byte("phone-session-id"))
	pcID := base64.StdEncoding.EncodeToString([]byte("pc-identity-1234"))
	if err := trustedAdults.remember(pairedPhone{RealDeviceID: "stable-phone", Key: key}); err != nil {
		t.Fatal(err)
	}
	reconnectEndpoint = newInviteWsData(pcID, "Linux", "control-pin", "fresh-endpoint-token", []string{"192.0.2.1"}, "", 19527)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, daemon, "--session", "--nofork", "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dbus.Connect(strings.TrimSpace(address))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	path := dbus.ObjectPath("/test/characteristic")
	const iface = "org.bluez.GattCharacteristic1"
	props, err := prop.Export(conn, path, prop.Map{iface: {
		"Value":     {Value: []byte{}, Emit: prop.EmitTrue},
		"Notifying": {Value: false, Emit: prop.EmitTrue},
	}})
	if err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 4)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(path), dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged")); err != nil {
		t.Fatal(err)
	}
	p := &blePeripheral{charProps: map[string]*prop.Properties{bleCharUUID2: props}}
	characteristic := &bleChar{owner: p, uuid: bleCharUUID2, props: props}
	if err := characteristic.StartNotify(); err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(map[string]any{"cmd": 0, "deviceId": phoneID, "serviceId": 19, "reqId": 23})
	request := negotiationTestFrame(t, key, phoneID, plain, true)
	if err := characteristic.WriteValue(request, map[string]dbus.Variant{"mtu": dbus.MakeVariant(uint16(516))}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case signal := <-signals:
			changed, ok := signal.Body[1].(map[string]dbus.Variant)
			if !ok {
				t.Fatal("invalid property-change signal")
			}
			value, ok := changed["Value"]
			if !ok {
				continue // subscription's Notifying update
			}
			out, ok := value.Value().([]byte)
			if !ok || len(out) < 45 || out[16] != 1 || base64.StdEncoding.EncodeToString(out[:16]) != pcID {
				t.Fatal("BLE reply is not an encrypted PC-origin frame")
			}
			decoded, _ := base64.StdEncoding.DecodeString(key)
			block, _ := aes.NewCipher(decoded)
			gcm, _ := cipher.NewGCM(block)
			plain, err := gcm.Open(nil, out[17:29], out[29:], out[:17])
			if err != nil {
				t.Fatal(err)
			}
			var endpoint inviteWsData
			if err := json.Unmarshal(plain, &endpoint); err != nil {
				t.Fatal(err)
			}
			if endpoint.Cmd != 1 || endpoint.ReqID != 23 || endpoint.Token != "fresh-endpoint-token" || endpoint.WsPort != 19527 {
				t.Fatal("BLE indication did not carry the correlated control endpoint")
			}
			return
		case <-deadline.C:
			t.Fatal("no encrypted endpoint Value signal reached the bus")
		}
	}
}
