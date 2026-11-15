// BLE (Bluetooth Low Energy) peripheral + scanner for rfproto, driven through
// BlueZ over D-Bus.
//
// Why this exists
// ---------------
// Live sessions (docs/05-route-a-crypto.md §12.1) pair fine over Wi-Fi/WS and
// then die with the phone showing "turn on bluetooth on both devices". The real
// PC never has that problem because it always runs a BLE stack:
//
//	framework/discovery/ble.onBleState (0x1806b7800)
//	  -> ble.startAdvertise (0x1806b7b80) whenever the adapter is powered
//	  -> framework/platform.BleStartAdvertise (0x18044afe0)   [cgo]
//	     -> NativeAPI.dll!ble_start_advertise (RVA 0x24cd0)
//	        -> BleServer.cpp: GattServiceProvider(0x5246) + characteristics,
//	           with the payload below attached to the advertising parameters
//
// Advertisement payload (recovered from ble.startAdvertise, winapi.dll):
//
//	byte 0    0x11                              (fixed tag)
//	byte 1    low byte of device cap            (device.Device+0x58, json "cap";
//	                                              the PC's default is 0x28ba -> 0xba)
//	byte 2..  base64decode(device.GetRandomDeviceId())   -- 16 bytes
//
// makeFgAdvData (negChannel, 0x180452f60) uses a 4-byte variant instead:
// 0x10 0x12 <u16 big-endian> + deviceId, and is what the PC *scans* for.
//
// The GATT service/characteristic UUIDs are literal GUIDs in NativeAPI.dll:
//
//	service          00005246-0000-1000-8000-00805f9b34fb   (VA 0x18007fe58)
//	characteristic   00000726-0000-1000-8000-00805f9b34fb   (VA 0x18007fe70)
//	characteristic   00000722-0000-1000-8000-00805f9b34fb   (VA 0x18007fe80)
//
// Phone-facing properties: Write=8, WriteWithoutResponse=4, Notify=16, Indicate=32.
// Paired phone negotiation uses a write-with-response and indication on 0x0722.
// 0x0726 is the optional write-without-response/notification data channel.
// Values are authenticated binary service-19 frames, not readable QR JSON.
// The advertisement identity is the same identity as mDNS and the QR.
package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

const (
	bluez          = "org.bluez"
	objManagerName = "org.freedesktop.DBus.ObjectManager"

	bleSvcUUID   = "00005246-0000-1000-8000-00805f9b34fb"
	bleSvcUUID16 = "5246"
	bleCharUUID1 = "00000726-0000-1000-8000-00805f9b34fb"
	bleCharUUID2 = "00000722-0000-1000-8000-00805f9b34fb"

	// bleAdvTag is byte 0 of the recovered ble.startAdvertise payload.
	bleAdvTag = 0x11
)

// bleCharRole describes the phone-facing GATT write/subscription contract.
type bleCharRole struct {
	uuid  string
	flags []string
}

func bleCharRoles() []bleCharRole {
	return []bleCharRole{
		{uuid: bleCharUUID2, flags: []string{"write", "indicate"}},
		{uuid: bleCharUUID1, flags: []string{"write-without-response", "notify"}},
	}
}

// bleAdvPayload builds the vendor payload the PC attaches to its advertisement.
func bleAdvPayload(deviceID string, capLow byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(deviceID)
	if err != nil {
		return nil, fmt.Errorf("deviceId %q is not base64: %w", deviceID, err)
	}
	out := make([]byte, 0, 2+len(raw))
	out = append(out, bleAdvTag, capLow)
	out = append(out, raw...)
	return out, nil
}

// bleConfig is everything the BLE peripheral needs.
type bleConfig struct {
	DeviceID  string // base64; same value as mDNS TXT deviceId= and QR wsData.deviceId
	Name      string // local name ("" = omit)
	CapLow    byte   // byte 1 of the payload (device cap low byte)
	Payload   []byte // overrides the built payload when non-nil
	WithGATT  bool   // also register the GATT application (service + 2 chars)
	LocalName bool   // include LocalName in the advertisement
	ShortUUID bool   // advertise 0x5246 in 16-bit form (fits legacy advertising)
}

func (c bleConfig) uuid() string {
	if c.ShortUUID {
		return bleSvcUUID16
	}
	return bleSvcUUID
}

// blePeripheral owns the exported D-Bus objects plus the registered BlueZ
// advertisement / GATT application.
type blePeripheral struct {
	conn     *dbus.Conn
	adapter  dbus.ObjectPath
	advPath  dbus.ObjectPath
	appPath  dbus.ObjectPath
	payload  []byte
	withGATT bool
	objects  map[dbus.ObjectPath]map[string]map[string]dbus.Variant

	mu        sync.Mutex
	charProps map[string]*prop.Properties
}

// BlueZ emits an ATT indication/notification when Value changes on a
// subscribed characteristic. SetMust updates local read-only properties;
// Properties.Set is the remote setter and would reject a read-only Value.
func (b *blePeripheral) setCharValue(uuid string, v []byte) *dbus.Error {
	b.mu.Lock()
	props := b.charProps[uuid]
	b.mu.Unlock()
	if props == nil {
		return dbus.NewError("org.bluez.Error.Failed", []interface{}{"characteristic unavailable"})
	}
	props.SetMust("org.bluez.GattCharacteristic1", "Value", v)
	return nil
}

// Release implements org.bluez.LEAdvertisement1.
func (b *blePeripheral) Release() *dbus.Error {
	log.Printf("[ble] advertisement released by BlueZ")
	return nil
}

// GetManagedObjects implements org.freedesktop.DBus.ObjectManager, which BlueZ
// requires on the GATT application root path.
func (b *blePeripheral) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return b.objects, nil
}

// bluezAdapter returns the first (lowest-pathed) adapter and its properties.
func bluezAdapter(conn *dbus.Conn) (dbus.ObjectPath, map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := conn.Object(bluez, "/").Call(objManagerName+".GetManagedObjects", 0).Store(&objs); err != nil {
		return "", nil, fmt.Errorf("GetManagedObjects: %w", err)
	}
	var best dbus.ObjectPath
	var bestProps map[string]dbus.Variant
	for p, ifaces := range objs {
		if a, ok := ifaces["org.bluez.Adapter1"]; ok {
			if best == "" || string(p) < string(best) {
				best, bestProps = p, a
			}
		}
	}
	if best == "" {
		return "", nil, fmt.Errorf("no org.bluez.Adapter1 found (is BlueZ running?)")
	}
	return best, bestProps, nil
}

// bleStart exports the advertisement (and optionally the GATT application) and
// registers both with BlueZ. It does not power the adapter on by itself.
func bleStart(cfg bleConfig) (*blePeripheral, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("system bus: %w", err)
	}
	adapter, aprops, err := bluezAdapter(conn)
	if err != nil {
		return nil, err
	}
	powered, _ := aprops["Powered"].Value().(bool)
	addr := ""
	if v, ok := aprops["Address"]; ok {
		addr = fmt.Sprintf("%v", v.Value())
	}
	log.Printf("[ble] adapter %s addr=%s powered=%v", adapter, addr, powered)
	if !powered {
		return nil, fmt.Errorf("adapter %s is not powered (run: bluetoothctl power on)", adapter)
	}

	payload := cfg.Payload
	if payload == nil {
		if payload, err = bleAdvPayload(cfg.DeviceID, cfg.CapLow); err != nil {
			return nil, err
		}
	}
	if len(payload) < 3 {
		return nil, fmt.Errorf("payload too short (%d B); expected [tag][cap][>=1 deviceId byte]", len(payload))
	}

	b := &blePeripheral{
		conn:     conn,
		adapter:  adapter,
		advPath:  "/rfproto/adv0",
		appPath:  "/rfproto/app",
		payload:  payload,
		withGATT: cfg.WithGATT,
		objects:  map[dbus.ObjectPath]map[string]map[string]dbus.Variant{},
	}

	uuid := cfg.uuid()
	advProps := prop.Map{
		"org.bluez.LEAdvertisement1": {
			"Type":           {Value: "peripheral", Emit: prop.EmitConst},
			"ServiceUUIDs":   {Value: []string{uuid}, Emit: prop.EmitConst},
			"ServiceData":    {Value: map[string]dbus.Variant{uuid: dbus.MakeVariant(append([]byte(nil), payload...))}, Emit: prop.EmitConst},
			"IncludeTxPower": {Value: false, Emit: prop.EmitConst},
		},
	}
	if cfg.LocalName && cfg.Name != "" {
		advProps["org.bluez.LEAdvertisement1"]["LocalName"] = &prop.Prop{Value: cfg.Name, Emit: prop.EmitConst}
	}
	if _, err := prop.Export(conn, b.advPath, advProps); err != nil {
		return nil, fmt.Errorf("export advertisement: %w", err)
	}
	if err := conn.Export(b, b.advPath, "org.bluez.LEAdvertisement1"); err != nil {
		return nil, fmt.Errorf("export LEAdvertisement1: %w", err)
	}

	call := conn.Object(bluez, adapter).Call("org.bluez.LEAdvertisingManager1.RegisterAdvertisement", 0, b.advPath, map[string]dbus.Variant{})
	if call.Err != nil {
		conn.Close()
		return nil, fmt.Errorf("RegisterAdvertisement: %w", call.Err)
	}
	log.Printf("[ble] advertising: svc=%s payload=%dB %s (tag=%#x cap=%#x deviceId=%s)",
		uuid, len(payload), hex.EncodeToString(payload), payload[0], payload[1],
		base64.StdEncoding.EncodeToString(payload[2:]))

	if cfg.WithGATT {
		if err := b.registerGATT(); err != nil {
			log.Printf("[ble] GATT application NOT registered: %v", err)
		} else {
			roles := bleCharRoles()
			log.Printf("[ble] GATT application registered: svc=%s", bleSvcUUID)
			for _, r := range roles {
				note := " (paired binary negotiation)"
				log.Printf("[ble]   char %s flags=%v%s", r.uuid, r.flags, note)
			}
		}
	}
	return b, nil
}

// registerGATT exports service 0x5246 and its two characteristics, then hands
// the application to BlueZ.
func (b *blePeripheral) registerGATT() error {
	svcPath := b.appPath + "/service0"
	svcIface := map[string]map[string]dbus.Variant{
		"org.bluez.GattService1": {
			"UUID":    dbus.MakeVariant(bleSvcUUID),
			"Primary": dbus.MakeVariant(true),
		},
	}
	b.objects[svcPath] = svcIface
	if _, err := prop.Export(b.conn, svcPath, prop.Map{
		"org.bluez.GattService1": {
			"UUID":    {Value: bleSvcUUID, Emit: prop.EmitConst},
			"Primary": {Value: true, Emit: prop.EmitConst},
		},
	}); err != nil {
		return fmt.Errorf("export service: %w", err)
	}

	for i, role := range bleCharRoles() {
		uuid := role.uuid
		charPath := b.appPath + dbus.ObjectPath(fmt.Sprintf("/char%d", i))
		val := []byte{}
		b.objects[charPath] = map[string]map[string]dbus.Variant{
			"org.bluez.GattCharacteristic1": {
				"UUID":    dbus.MakeVariant(uuid),
				"Service": dbus.MakeVariant(svcPath),
				"Flags":   dbus.MakeVariant(role.flags),
				"Value":   dbus.MakeVariant(val),
			},
		}
		cprops, err := prop.Export(b.conn, charPath, prop.Map{
			"org.bluez.GattCharacteristic1": {
				"UUID":           {Value: uuid, Emit: prop.EmitConst},
				"Service":        {Value: svcPath, Emit: prop.EmitConst},
				"Flags":          {Value: role.flags, Emit: prop.EmitConst},
				"Value":          {Value: val, Emit: prop.EmitTrue},
				"Notifying":      {Value: false, Emit: prop.EmitTrue},
				"WriteAcquired":  {Value: false, Emit: prop.EmitConst},
				"NotifyAcquired": {Value: false, Emit: prop.EmitConst},
			},
		})
		if err != nil {
			return fmt.Errorf("export characteristic %s: %w", uuid, err)
		}
		b.mu.Lock()
		if b.charProps == nil {
			b.charProps = map[string]*prop.Properties{}
		}
		b.charProps[uuid] = cprops
		b.mu.Unlock()
		if err := b.conn.Export(&bleChar{owner: b, uuid: uuid, flags: role.flags, props: cprops}, charPath, "org.bluez.GattCharacteristic1"); err != nil {
			return fmt.Errorf("export GattCharacteristic1 %s: %w", uuid, err)
		}
	}

	if err := b.conn.Export(b, b.appPath, objManagerName); err != nil {
		return fmt.Errorf("export ObjectManager: %w", err)
	}
	if c := b.conn.Object(bluez, b.adapter).Call("org.bluez.GattManager1.RegisterApplication", 0, b.appPath, map[string]dbus.Variant{}); c.Err != nil {
		return fmt.Errorf("RegisterApplication: %w", c.Err)
	}
	return nil
}

// Stop unregisters the advertisement/application and closes the bus.
func (b *blePeripheral) Stop() {
	if b == nil || b.conn == nil {
		return
	}
	obj := b.conn.Object(bluez, b.adapter)
	if b.withGATT {
		if c := obj.Call("org.bluez.GattManager1.UnregisterApplication", 0, b.appPath); c.Err != nil {
			log.Printf("[ble] UnregisterApplication: %v", c.Err)
		}
	}
	if c := obj.Call("org.bluez.LEAdvertisingManager1.UnregisterAdvertisement", 0, b.advPath); c.Err != nil {
		log.Printf("[ble] UnregisterAdvertisement: %v", c.Err)
	}
	b.conn.Close()
}

// bleChar implements the characteristic methods BlueZ needs.
type bleChar struct {
	owner *blePeripheral
	uuid  string
	flags []string
	props *prop.Properties
}

// WriteValue authenticates the paired request before publishing a correlated,
// encrypted endpoint reply on the same subscribed characteristic.
func (c *bleChar) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	log.Printf("[ble] %s WriteValue %dB", c.uuid, len(value))
	plain, aead, peerID, err := openNegotiation(value, trustedAdults)
	if err != nil {
		log.Printf("[ble] negotiation rejected: %v", err)
		return dbus.NewError("org.bluez.Error.NotAuthorized", []interface{}{"negotiation authentication failed"})
	}
	out, reqID, err := endpointReply(value, plain, aead)
	if err != nil {
		log.Printf("[ble] endpoint reply: %v", err)
		return dbus.NewError("org.bluez.Error.Failed", []interface{}{"invalid negotiation request"})
	}
	if v, ok := options["mtu"]; ok {
		if mtu, ok := v.Value().(uint16); ok && len(out) > int(mtu)-3 {
			return dbus.NewError("org.bluez.Error.InvalidValueLength", []interface{}{"endpoint exceeds negotiated MTU"})
		}
	}
	if err := c.owner.setCharValue(c.uuid, out); err != nil {
		return err
	}
	log.Printf("[ble] authenticated phone=%s reqId=%d; encrypted reply=%dB", peerID, reqID, len(out))
	return nil
}

func (c *bleChar) StartNotify() *dbus.Error {
	log.Printf("[ble] %s StartNotify", c.uuid)
	c.props.SetMust("org.bluez.GattCharacteristic1", "Notifying", true)
	return nil
}

func (c *bleChar) StopNotify() *dbus.Error {
	log.Printf("[ble] %s StopNotify", c.uuid)
	c.props.SetMust("org.bluez.GattCharacteristic1", "Notifying", false)
	return nil
}

// BlueZ owns the ATT indication acknowledgment state; this callback records
// successful confirmation from the peer.
func (c *bleChar) Confirm() *dbus.Error {
	log.Printf("[ble] %s indication confirmed", c.uuid)
	return nil
}

// ---------- scanning ----------

// bleScan watches LE advertisements for secs, logging everything and decoding
// the Ready For service data (0x5246) when present.
func bleScan(secs int, matchID string) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	defer conn.Close()
	adapter, aprops, err := bluezAdapter(conn)
	if err != nil {
		return err
	}
	log.Printf("[ble] scanning on %s addr=%v for %ds", adapter, aprops["Address"].Value(), secs)
	adapterObj := conn.Object(bluez, adapter)

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/"),
		dbus.WithMatchInterface(objManagerName),
		dbus.WithMatchMember("InterfacesAdded"),
	); err != nil {
		return fmt.Errorf("AddMatchSignal: %w", err)
	}
	sig := make(chan *dbus.Signal, 256)
	conn.Signal(sig)

	if c := adapterObj.Call("org.bluez.Adapter1.SetDiscoveryFilter", 0, map[string]dbus.Variant{
		"Transport":     dbus.MakeVariant("le"),
		"DuplicateData": dbus.MakeVariant(true),
	}); c.Err != nil {
		log.Printf("[ble] SetDiscoveryFilter: %v", c.Err)
	}
	if c := adapterObj.Call("org.bluez.Adapter1.StartDiscovery", 0); c.Err != nil {
		return fmt.Errorf("StartDiscovery: %w", c.Err)
	}
	defer func() {
		if c := adapterObj.Call("org.bluez.Adapter1.StopDiscovery", 0); c.Err != nil {
			log.Printf("[ble] StopDiscovery: %v", c.Err)
		}
	}()

	deadline := time.After(time.Duration(secs) * time.Second)
	n := 0
	for {
		select {
		case <-deadline:
			log.Printf("[ble] scan done (%d advert(s))", n)
			return nil
		case s := <-sig:
			if s == nil || s.Name != objManagerName+".InterfacesAdded" || len(s.Body) < 2 {
				continue
			}
			path, _ := s.Body[0].(dbus.ObjectPath)
			ifaces, ok := s.Body[1].(map[string]map[string]dbus.Variant)
			if !ok {
				continue
			}
			dev, ok := ifaces["org.bluez.Device1"]
			if !ok {
				continue
			}
			n++
			logDevice(path, dev, matchID)
		}
	}
}

func logDevice(path dbus.ObjectPath, dev map[string]dbus.Variant, matchID string) {
	str := func(k string) string {
		if v, ok := dev[k]; ok {
			return fmt.Sprintf("%v", v.Value())
		}
		return ""
	}
	log.Printf("[ble] adv %s addr=%s rssi=%v name=%q alias=%q", path, str("Address"), str("RSSI"), str("Name"), str("Alias"))
	if list, ok := dev["UUIDs"].Value().([]string); ok && len(list) > 0 {
		log.Printf("[ble]   uuids= %s", strings.Join(list, " "))
	}
	if m, ok := dev["ServiceData"].Value().(map[string]dbus.Variant); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			raw, _ := m[k].Value().([]byte)
			log.Printf("[ble]   serviceData %s = %s", k, hex.EncodeToString(raw))
			if strings.EqualFold(strings.TrimPrefix(k, "0x"), bleSvcUUID16) || strings.EqualFold(k, bleSvcUUID) {
				decodeReadyForPayload(raw, matchID)
			}
		}
	}
	if m, ok := dev["ManufacturerData"].Value().(map[uint16]dbus.Variant); ok {
		for cid, v := range m {
			raw, _ := v.Value().([]byte)
			log.Printf("[ble]   manufacturerData 0x%04X = %s", cid, hex.EncodeToString(raw))
		}
	}
}

// decodeReadyForPayload interprets [tag][cap][deviceId...] as advertised by a
// Ready For peer.
func decodeReadyForPayload(raw []byte, matchID string) {
	if len(raw) < 3 {
		log.Printf("[ble]   -> ReadyFor payload too short")
		return
	}
	b64 := base64.StdEncoding.EncodeToString(raw[2:])
	note := ""
	switch {
	case matchID == "":
	case b64 == matchID:
		note = " MATCHES -id"
	default:
		note = " (does not match -id=" + matchID + ")"
	}
	log.Printf("[ble]   -> ReadyFor tag=%#x cap=%#x deviceId=%s%s", raw[0], raw[1], b64, note)
}
