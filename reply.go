// Server-side pairing reply for rfproto.
//
// This mirrors what the Windows PC sends back to a device that has just
// connected and announced itself. The structure below was recovered from the
// reflect metadata embedded in winapi.dll (see docs/05-route-a-crypto.md §12);
// every json tag and field offset is taken from the binary, not guessed.
//
// Go types (winapi.dll VAs) and their fields:
//
//	outer  websocket.WebsocketMsg          @ 0x180b4a460  (size 0x80)
//	  +0x00 SrcDeviceId   string   `json:"deviceId,omitempty"`
//	  +0x10 DstDeviceId   string   `json:"-"`
//	  +0x20 SrcServiceId  int      `json:"srcServiceId"`
//	  +0x28 DstServiceId  int      `json:"serviceId"`
//	  +0x30 MsgId         uint64   `json:"msgId,omitempty"`
//	  +0x38 Result        int      `json:"-"`
//	  +0x40 LocalIp       string   `json:"-"`
//	  +0x50 RemoteIp      string   `json:"-"`
//	  +0x60 Data          []byte   `json:"data,omitempty"`   (json.RawMessage)
//	  +0x78 OnResult      func     `json:"-"`
//
//	data   (the reply payload)             @ 0x180b4eae0  (size 0x80)
//	  +0x00 Version         int        `json:"ver"`
//	  +0x08 Cmd             int        `json:"cmd"`
//	  +0x10 Token           string     `json:"token,omitempty"`
//	  +0x20 Paired          int        `json:"paired,omitempty"`
//	  +0x28 RemoteIp        string     `json:"ip,omitempty"`
//	  +0x38 ConnType        int        `json:"connType,omitempty"`
//	  +0x40 RemoteServers   []*Remote  `json:"ips,omitempty"`
//	  +0x58 ReadyforDevice  *Device    `json:"device,omitempty"`
//	  +0x60 ServiceVersions map[int]int `json:"serviceVersions,omitempty"`
//	  +0x68 ApkVerCode      int        `json:"apkVerCode,omitempty"`
//	  +0x70 ApkVerName      string     `json:"apkVerName,omitempty"`
//
//	device.Device (data.device)            @ 0x180b72ee0  (size 0xe8)
//	  +0x00 Id            string    `json:"deviceId,omitempty"`
//	  +0x10 RealId        string    `json:"realDeviceId,omitempty"`
//	  +0x20 Name          string    `json:"deviceName,omitempty"`
//	  +0x30 Type          uint8     `json:"type,omitempty"`
//	  +0x31 Os            uint8     `json:"os,omitempty"`
//	  +0x32 ScType        uint8     `json:"scType,omitempty"`
//	  +0x38 Sn            string    `json:"sn,omitempty"`
//	  +0x48 Sku           string    `json:"Sku,omitempty"`
//	  +0x58 Capability    uint16    `json:"cap,omitempty"`
//	  +0x60 Capabilities  map[int]int `json:"caps,omitempty"`
//	  +0x70 Channel       int       `json:"ch,omitempty"`
//	  +0x78 Version       int       `json:"ver,omitempty"`
//	  +0x80 ServiceVersions map[int]int `json:"serviceVersions,omitempty"`
//	  +0x88 IsPaired      bool      `json:"isPaired,omitempty"`
//	  +0x90 State         int       `json:"state,omitempty"`
//	  +0x98 Online        bool      `json:"online,omitempty"`
//	  +0xa0 Key           string    `json:"key,omitempty"`
//	  +0xb0 VerCode       int64     `json:"verCode,omitempty"`
//	  +0xb8 VerName       string    `json:"verName,omitempty"`
//	  +0xc8 MinVerCode    map[int]int `json:"minVerCode,omitempty"`
//	  +0xd0 CloudPush     int       `json:"cloudPush,omitempty"`
//	  +0xd8 FoldType      int       `json:"foldType,omitempty"`
//	  +0xe0 PairedVia     int       `json:"pairedVia,omitempty"`
//
// Every literal value the real PC puts in that struct is installed by
// device.CurrentDevice (0x18030dfe0), which copies the fields onto the stack at
// rsp+0x28. That single function proves the offset mapping: Capability is the
// only 16-bit store (movw), and VerCode/VerName/MinVerCode land at +0xb0/+0xb8/
// +0xc8 from the globals 0x1813505f8, 0x1813a0400 and 0x1813cefd8.
//
//	Device+0x30  movb $0x4                     -> Type = 4 (PC)
//	Device+0x32  movb $0x2                     -> ScType = 2
//	Device+0x58  movzwl + 0x18141c4f4          -> cap = pcCapability
//	Device+0x60  mov 0x1813cf088               -> caps = pcCapabilities
//	Device+0x80  mov 0x1813cf080               -> serviceVersions = pcServiceVersions
//	Device+0xb0  0x1813505f8                   -> verCode = pcVerCode
//	Device+0xb8  0x1813a0400/08                -> verName = pcVerName
//	Device+0xc8  0x1813cefd8                   -> minVerCode = pcMinVerCode
//
// The three map fields are the same reflect kind in the type descriptor, so
// caps/serviceVersions/minVerCode are all maps — caps is NOT a list.
//
// Sn and Sku are not decoration: a live run with adb logcat attached proved the
// phone inserts the whole object into its SQLCipher table `remote_device`, whose
// `sn` column is NOT NULL. Without it the insert fails, the follow-on
// version-matching path then dereferences a null and the app crashes back to
// the scan screen ("insert RemoteDevice error! SQLiteConstraintException: NOT
// NULL constraint failed: remote_device.sn" -> FATAL EXCEPTION ->
// AddDevicesActivity). See identity.go for where the two values come from:
//
//	+0x38 Sn   device.GetDeviceSn (0x18030e280)        -> MachineGuid
//	+0x48 Sku  device.getDeviceHardwareInfo (0x18030e9c0) -> SystemProductName
//
//	ips element (remote server)            @ 0x180b15ea0  (size 0x38)
//	  +0x00 ConnType    `json:"connType,omitempty"`
//	  +0x08 Ip          `json:"ip,omitempty"`
//	  +0x18 Port        `json:"port,omitempty"`
//	  +0x20 Fingerprint `json:"fp,omitempty"`
//
// In websocketServer.readyforHandler the PC builds exactly this and writes it
// with gorilla websocket Conn.WriteJSON after validating the peer's
// deviceId+token (log: "DeviceId(%s) token(%s) is OK, IsPaired:%d ..."). The
// observed literal assignments (winapi 0x1807b09e0..0x1807b0d90) are:
//
//	data.ver              = 3
//	data.token            = "OK"      (literal at 0x180b8eb64, len 2)
//	data.paired           = <paired flag>
//	data.connType         = server.connType
//	data.serviceVersions  = server.serviceVersions (a map[int]int; the PC's own
//	                        map is pcServiceVersions)
//	data.device.deviceId  = device.GetRandomDeviceId()
//	data.device.realId    = device.GetDeviceId()
//	data.device.key       = <pair key>
//	data.device.isPaired  = <bool>
//	outer.deviceId        = server.deviceId
//	outer.srcServiceId    = 0
//	outer.serviceId       = 0
//	outer.msgId           = 0 (omitted from JSON)
package main

import "encoding/json"

// pcRemoteServer is the element type of data.ips.
type pcRemoteServer struct {
	ConnType int    `json:"connType,omitempty"`
	IP       string `json:"ip,omitempty"`
	Port     int    `json:"port,omitempty"`
	FP       string `json:"fp,omitempty"`
}

// pcDevice mirrors the PC's device.Device struct carried in data.device.
type pcDevice struct {
	DeviceID        string      `json:"deviceId,omitempty"`
	RealDeviceID    string      `json:"realDeviceId,omitempty"`
	DeviceName      string      `json:"deviceName,omitempty"`
	Type            int         `json:"type,omitempty"`
	OS              int         `json:"os,omitempty"`
	ScType          int         `json:"scType,omitempty"`
	SN              string      `json:"sn,omitempty"`
	SKU             string      `json:"Sku,omitempty"`
	SKUPhone        string      `json:"sku,omitempty"`  // A/B probe, see replyInfo.SKUKey
	Capability      int         `json:"cap,omitempty"`  // uint16 on the wire
	Capabilities    map[int]int `json:"caps,omitempty"` // serviceId -> capability level
	Channel         int         `json:"ch,omitempty"`
	Version         int         `json:"ver,omitempty"`
	ServiceVersions map[int]int `json:"serviceVersions,omitempty"`
	IsPaired        bool        `json:"isPaired,omitempty"`
	State           int         `json:"state,omitempty"`
	Online          bool        `json:"online,omitempty"`
	Key             string      `json:"key,omitempty"`
	VerCode         int         `json:"verCode,omitempty"`
	VerName         string      `json:"verName,omitempty"`
	MinVerCode      map[int]int `json:"minVerCode,omitempty"`
	CloudPush       int         `json:"cloudPush,omitempty"`
	FoldType        int         `json:"foldType,omitempty"`
	PairedVia       int         `json:"pairedVia,omitempty"`
}

// pcDiscoveryData mirrors the `data` payload of the server reply.
type pcDiscoveryData struct {
	Ver             int              `json:"ver"`
	Cmd             int              `json:"cmd"`
	Token           string           `json:"token,omitempty"`
	Paired          int              `json:"paired,omitempty"`
	RemoteIP        string           `json:"ip,omitempty"`
	ConnType        int              `json:"connType,omitempty"`
	RemoteServers   []pcRemoteServer `json:"ips,omitempty"`
	Device          *pcDevice        `json:"device,omitempty"`
	ServiceVersions map[int]int      `json:"serviceVersions,omitempty"`
	ApkVerCode      int              `json:"apkVerCode,omitempty"`
	ApkVerName      string           `json:"apkVerName,omitempty"`
}

// pcDiscoveryMsg mirrors websocket.WebsocketMsg, the outer envelope.
// srcServiceId/serviceId have no omitempty, so they are always emitted.
type pcDiscoveryMsg struct {
	DeviceID     string           `json:"deviceId,omitempty"`
	SrcServiceID int              `json:"srcServiceId"`
	ServiceID    int              `json:"serviceId"`
	MsgID        int64            `json:"msgId,omitempty"`
	Data         *pcDiscoveryData `json:"data,omitempty"`
}

// replyInfo carries our own identity into the reply builder.
type replyInfo struct {
	DeviceID string
	Name     string
	FP       string
	IP       string
	Port     int
	Token    string // data.token; the PC sends the literal "OK"
	Paired   bool
	Key      string // authenticated peer's persisted shared key

	// SN is device.Device+0x38 (json "sn"). The PC fills it from the Windows
	// MachineGuid; without a non-empty value the phone's remote_device insert
	// fails. See identity.go.
	SN string
	// SKU is device.Device+0x48. The field's json tag in winapi.dll is
	// literally `Sku,omitempty` (capital S), but the phone spells the same
	// value `sku` in every frame it sends. SKUKey picks which key we emit so
	// the two can be A/B'd against the phone's logcat without a rebuild:
	// "Sku" (faithful to winapi.dll), "sku" (the phone's spelling), "both".
	SKU    string
	SKUKey string
}

// pcCapability is the PC's capability bitmask, a uint16 at device.Device+0x58
// (json "cap"). device.init.0 (0x18030ee00) installs it with
// `movw $0x28ba,0x18141c4f4`; device.ResetCapByRegSetting (0x18030f2c0) then
// clears individual bits when the matching registry switch is off
// (RemoteDesktopMode -> bit 1, Webcam -> bit 3, RemotePCLock -> ...).
//
// 0x28ba = 10426, so the low byte the BLE advert must carry is 0xba.
const pcCapability = 0x28ba

// pcCapabilities is device.Device+0x60 (json "caps"): a map from serviceId to a
// capability level, written by device.CurrentDevice from the global 0x1813cf088
// and populated by device.init.0.
var pcCapabilities = map[int]int{
	1: 2, 2: 3, 3: 2, 5: 2, 9: 2, 13: 2, 15: 2, 20: 2,
	22: 1, 26: 2, 204: 2, 205: 3, 206: 3,
}

// pcServiceVersions is device.Device+0x80 (json "serviceVersions"), written by
// device.CurrentDevice from the global 0x1813cf080 and populated by
// device.init.0 as an explicit 20-entry map literal.
var pcServiceVersions = map[int]int{
	0: 3, 1: 10, 2: 5, 3: 3, 5: 2, 9: 4, 13: 1, 15: 16, 18: 1, 20: 4,
	21: 3, 22: 2, 24: 2, 25: 1, 26: 1, 32: 2, 36: 1, 204: 1, 205: 2, 206: 1,
}

// pcMinVerCode is device.Device+0xc8 (json "minVerCode"), built as a map literal
// by common/constants.init (0x180188000) into the global 0x1813cefd8
// (movb <key> then `movq $0x6acfc0/$0x29b92700`).
var pcMinVerCode = map[int]int{1: 7000000, 2: 700000000, 3: 7000000, 4: 7000000}

// The PC's own build identity: device.Device+0xb0/+0xb8 (json verCode/verName)
// come from the globals 0x1813505f8 and 0x1813a0400 (a Go string).
const (
	pcVerCode = 900001000
	pcVerName = "09.0.0.001.000"
)

// ourDevice builds the device.Device object the PC presents in replies.
func ourDevice(ri replyInfo) *pcDevice {
	dev := &pcDevice{
		DeviceID:        ri.DeviceID,
		RealDeviceID:    ri.DeviceID,
		DeviceName:      ri.Name,
		Type:            4, // device.CurrentDevice: movb $0x4 -> Device+0x30
		OS:              6, // Windows (matches the QR wsData.os)
		ScType:          2, // device.CurrentDevice: movb $0x2 -> Device+0x32
		SN:              ri.SN,
		Capability:      deviceCapability,
		Capabilities:    pcCapabilities,
		Version:         2,
		ServiceVersions: pcServiceVersions,
		IsPaired:        ri.Paired,
		State:           1,
		Online:          true,
		VerCode:         pcVerCode,
		VerName:         pcVerName,
		MinVerCode:      pcMinVerCode,
		Key:             ri.Key,
	}
	// Sku vs sku: winapi.dll tags the field `Sku,omitempty`, the phone spells
	// it `sku`. Default to the recovered tag and let -skukey A/B it.
	switch ri.SKUKey {
	case "sku":
		dev.SKUPhone = ri.SKU
	case "both":
		dev.SKU, dev.SKUPhone = ri.SKU, ri.SKU
	default: // "Sku", "" (faithful)
		dev.SKU = ri.SKU
	}
	return dev
}

// buildDiscoveryReply assembles the JSON the PC writes back on the first
// message from a connecting device.
func buildDiscoveryReply(ri replyInfo) ([]byte, error) {
	if ri.Token == "" {
		ri.Token = "OK"
	}
	dev := ourDevice(ri)
	var ips []pcRemoteServer
	if ri.IP != "" {
		ips = []pcRemoteServer{{ConnType: 1, IP: ri.IP, Port: ri.Port, FP: ri.FP}}
	}
	msg := &pcDiscoveryMsg{
		DeviceID: ri.DeviceID,
		Data: &pcDiscoveryData{
			Ver:             3,
			Cmd:             0,
			Token:           ri.Token,
			Paired:          boolInt(ri.Paired),
			RemoteIP:        ri.IP,
			ConnType:        1,
			RemoteServers:   ips,
			Device:          dev,
			ServiceVersions: pcServiceVersions,
			ApkVerCode:      pcVerCode,
			ApkVerName:      pcVerName,
		},
	}
	return json.Marshal(msg)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// pairRequest is the subset of a manager (serviceId 0) message needed to spot
// the phone's "pair confirm" and pull the shared key out of it.
type pairRequest struct {
	ServiceID    *int `json:"serviceId"`
	SrcServiceID *int `json:"srcServiceId"`
	Data         struct {
		Cmd    *int `json:"cmd"`
		Paired *int `json:"paired"`
		Device *struct {
			DeviceID     string `json:"deviceId"`
			RealDeviceID string `json:"realDeviceId"`
			Key          string `json:"key"`
		} `json:"device"`
	} `json:"data"`
}

// pairKeyFrom reports whether msg is the phone's pair-confirm (manager
// serviceId 0, data.cmd == 1) and returns the shared key it carries.
//
// In winapi.dll this lands in websocket.onMessage (0x1807a6ba0), whose cmd==1
// branch sets device.IsPaired, calls device.PaireDevice(device, key) and then
// marshals a reply built from device.CurrentDevice() with its Key field
// (device.Device+0xa0, json "key") overwritten by the incoming key.
func pairKeyFrom(msg []byte) (key string, ok bool) {
	var pr pairRequest
	if json.Unmarshal(msg, &pr) != nil {
		return "", false
	}
	if pr.ServiceID == nil || *pr.ServiceID != 0 || pr.SrcServiceID == nil || *pr.SrcServiceID != 0 {
		return "", false
	}
	if pr.Data.Cmd == nil || *pr.Data.Cmd != 1 {
		return "", false
	}
	if pr.Data.Device == nil {
		return "", false
	}
	return pr.Data.Device.Key, true
}

// buildPairAck assembles the PC's reply to a pair-confirm: a manager
// (serviceId 0) message whose payload is {ver:3, cmd:1|2, device:<ours + key>}.
//
// Recovered from winapi.dll websocket.onMessage (0x1807a6ba0,
// 0x1807a7320..0x1807a7512):
//
//	data.ver = 3                       (literal; the only value ever stored)
//	data.cmd = 1                       (device.PaireDevice returned true)
//	         = 2 when PaireDevice hit the paired-device cap and returned false
//	data.device = *device.CurrentDevice()
//	data.device.key = <incoming key>   (copied from the request's device.key)
//	outer.msgId = ++globalMsgId        (a process-wide counter)
//
// It is sent on serviceId 0, matching websocket.PaireDeviceWithPin
// (0x1807a67c0), which ends in websocket.SendServiceMsg(0).
func buildPairAck(ri replyInfo, key string, cmd int) ([]byte, error) {
	if cmd == 0 {
		cmd = 1
	}
	dev := ourDevice(ri)
	dev.IsPaired = cmd == 1
	dev.Key = ""
	if dev.IsPaired {
		dev.Key = key
	}
	msg := &pcDiscoveryMsg{
		DeviceID: ri.DeviceID,
		Data: &pcDiscoveryData{
			Ver:    3,
			Cmd:    cmd,
			Device: dev,
		},
	}
	return json.Marshal(msg)
}
