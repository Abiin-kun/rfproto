# Route A findings — static recovery of discovery/pairing crypto (2026-10-15)

Method: Go 1.25 pclntab parsing (custom `gopclntab`/`gomap.py`: 23,096 functions mapped),
Ghidra 12.1.3 headless decompile of 140 target functions (all app functions renamed in-DB),
objdump cross-check of crypto cores. All literals/addresses are observations from `winapi.dll`.

## 1. PIN KDF — `pingToKey` (0x180446b00) — EXACT

```
pingToKey(pin uint32) string:
    buf[32] = bswap32(pin) repeated 8 times
    return base64.StdEncoding.EncodeToString(buf)   // 44 chars
```

- `SetPin` (0x180448340): `setDeviceTempKey(pingToKey(pin))` + log.
- `IsMatchPin` compares stored temp-key strings; `getDeviceIdPin` looks up per-device PIN state.
- Note: no hash — the 32-byte "key" is the PIN repeated. Short-lived temp keys only.

## 2. Discovery-message cipher — AES-256-GCM, stdlib — EXACT wire format

Call chain (assembly-verified): `base64.DecodeString(key)` → `aes.NewCipher` →
`cipher.NewGCM` → `gcm.Seal` / `gcm.Open` (itab slots), `additionalData = nil`.

This older discovery cipher is **not** the paired endpoint-negotiation frame.
LAN `/readyfor/mdns` and BLE service-19 negotiation use authenticated header AAD;
see §13. Do not apply the nil-AAD description below to those reconnect messages.

**Encrypt** (0x180446d20):
1. key = `getDeviceTempKey()` (unpaired) or `GetPairedDeviceKey()` / `GetDeviceSharedKey()` (paired).
2. `keyRaw = base64decode(key)` (32 B → AES-256; `"NewGCM base64Key:%s  err -> %s"`).
3. `nonce = GenRandomNonce()` = **12 bytes from math/rand** (NOT crypto/rand):
   `makeslice(0xc)`, `bswap(rand.Uint64()) || bswap(uint32(rand.Int63()>>31))`, seeded with `time.Now`.
4. `out = Seal(nonce, nonce, plaintext, nil)` idiom → output embeds the nonce.
5. returns `string(sealed)`; logs `"encryptd Data:%s"`.

**Decrypt** (0x180447760) framing — slice arithmetic verified in assembly:
```
msg = raw bytes, len >= 29 (0x1d), else error
tag16   = msg[0:16]          # key-lookup tag, base64'd for the temp-key map
msgType = msg[16]            # 0x02 = key-exchange/JSON, 0x03 = unpair/remove-temp-key, else data
nonce   = msg[17:29]         # 12 bytes (0x11=17, 0xc=12)
ct      = msg[29:]
pt      = gcm.Open(nil, nonce, ct, nil) using base64decode(paired-or-temp-or-shared key)
```
- Entry logs each input byte as hex: `"RAW Data:%s"` (`"%.2X"` per byte).
- Post-decrypt: `json.Valid(pt)` gate; type 0x02 path calls `setDeviceTempKey` (pairing key install).

Python reference (independent reimplementation):
```python
import base64, hashlib
from Crypto.Cipher import AES  # PyCryptodome
def ping_to_key(pin: int) -> str:
    b = pin.to_bytes(4, 'little') * 8   # bswap32(pin) repeated == little-endian repeat
    return base64.b64encode(b).decode()
key = base64.b64decode(ping_to_key(pin))          # 32 B
def encrypt(key, plaintext: bytes, tag16: bytes, mtype: int, nonce: bytes) -> bytes:
    ct = AES.new(key, AES.MODE_GCM, nonce=nonce).encrypt_and_digest(plaintext)[0] \
         + AES.new(key, AES.MODE_GCM, nonce=nonce).digest()  # Seal appends 16-B tag
    return tag16 + bytes([mtype]) + nonce + ct
```
(`pingToKey` byte order note: 8× bswap32(pin) big-endian-fill equals `pin.to_bytes(4,'little')*8`
as a byte string — verify against one live sample before shipping.)

## 3. mDNS TXT schema — EXACT (8 keys)

`mdns.toText` (0x1808f81e0) builds TXT via `fmt.Sprintf` (format strings extracted with lengths):
`deviceId=%s`, `deviceName=%s`, `type=%d`, `os=%d`, `cap=%d`, `timestamp=%d`, `fp=%s`, `appType=%d`
- PC mDNS `fp` = **hex SHA-256 of leaf DER**, as required by the phone's LAN
  negotiation validator. QR/endpoint `fp` is base64; phone TXT uses hex `Cfp`.
- Service **`_readyformdns._tcp`** [wire-confirmed §9]; a second `_readyfor._tcp.` string also exists
  in the binary; companion name `smartconnect-pcp2p`.

## 4. BLE service UUIDs — RECOVERED (U1 closed)

PC acts as a **GATT peripheral** (`GattServiceProvider`, `BleServer.cpp`; `BleStartAdvertise`
0x18044afe0 forwards raw adv bytes to NativeAPI `ble_start_advertise` 0x180024cd0).
Three custom 16-bit UUIDs as full 128-bit BT-base GUIDs (NativeAPI.dll `.rdata`, `movups`-loaded):

| VA | GUID | short | role |
|---|---|---|---|
| `0x18007fe90` | `00005246-0000-1000-8000-00805f9b34fb` | **0x5246** ("RF" = Ready For) | GATT **service** UUID — stored into the `BleServer` object at construction (`FUN_180052780`, `lea`+store), passed to `CreateAsync` |
| `0x18007fe80` | `00000722-0000-1000-8000-00805f9b34fb` | 0x0722 | characteristic #1 — `movups`-loaded and passed to the same COM method twice in `FUN_180052780` (two `CreateCharacteristicAsync` calls, inferred) |
| `0x18007fe70` | `00000726-0000-1000-8000-00805f9b34fb` | 0x0726 | characteristic #2 — same pattern |

Scan side (`BleWatcher.cpp`, `FUN_18004fd30`) matches received advertisements against
0x0722/0x0726 with 16-byte `sub`+`jne` comparisons.

FG advertisement payload (`negChannel.(*connection).makeFgAdvData` 0x180452f60) — EXACT:
```
adv = [0x10, 0x12, u16BE(port-or-type), ...base64decode(deviceIdString)]
```
i.e. 4-byte header + raw device-ID bytes.

The phone-facing GATT contract is now established: `0722` write + indicate,
`0726` write-without-response + notify. The advertisement is service data for
UUID `0x5246`; host HCI capture confirms the documented compact payload.
See §13 for the authenticated value format and successful reconnect.

## 5. TLS pinning — EXACT scheme

- `FingerprintToBase64(s) = base64(hex.DecodeString(s))` (0x1802feba0).
- `verifyRemoteServers`: fan-out goroutine per candidate server → TLS dial →
  `sha256(peerCertDER)` → base64 → string-compare with expected fp
  (`"verify remote server result: %t, ip: %s, connType: %d"`,
  `"fingerprint :%s, certificate fp:%s"`).
- `SelfSign`/`selfSign`/`ParseCrt`: per-machine self-signed cert for the local HTTPS server.

## 6. Pairing message flow — STRUCTURE (field names TBD)

`websocket.PaireDeviceWithPin` (0x1807a67c0):
1. `discovery.FindDeviceId()` → 2. `message.SetPin(pin)` → 3. `GenRandomKey()` →
   4. struct{device, key} → 5. `json.Marshal` → 6. `websocket.SendServiceMsg`.
- Pairing/QR vocabulary (json tags observed): `pinCode`, `pairedVia`, `invitePort`,
  `realDeviceId`, `newDeviceId`/`oldDeviceId`, `isPaired`, `triggerWay`, `foldType`.
- `services/QrCode`: `GetInviteMsg` = `json.Marshal` of invite struct over `genRandomQrDeviceId`;
  hotspot QR carries `MobileHotspot.QrCodeMsg{..., pwd}`.

## 7. Pipe codec — CLOSED (U3)

gob (`encoding/gob` ×381, `gob.CommonType`); 22-export `rfa_rpc_*` C ABI enumerated (E7).

---

# Route B evidence — live capture (win/ folder, 2026-10-15)

Live artifacts supplied by the user: 4 QR screenshots + a 2559-packet pcapng from a real
Windows pairing session against a `moto g34 5G`. Raw evidence saved to `raw/qr_decoded.txt`
and `raw/pcap_analysis.txt` (re-runnable tools: `raw/qr_decode.go.txt`, `raw/pcap_topo.go.txt`).

## 8. QR invite payload — EXACT (U2 QR closed)

All 4 QRs decoded (q2/q3 needed 4× upscale). The QR is **NOT pure JSON** — it is a literal
ASCII prefix `motorolardpconnection` immediately followed by a JSON object:

```
motorolardpconnection{"version":"7.0.0","wsData":{...},"ips":[...]}
```

`wsData` fields (exact, two independent sessions q1–q3 vs qr4):

| field | example | meaning |
|---|---|---|
| `ver` | 2 | envelope version |
| `cmd` | 1 | command id |
| `deviceId` | `KIfv22hgTCCANmbV3jiOTg==` | PC device id (base64, STABLE across sessions) |
| `deviceName` | `DESKTOP-PDPENKE` | PC hostname |
| `fp` | `B4EC2L5m...` / `KGxzb+wJ...` | PC cert fingerprint, **base64** — DIFFERS per session |
| `token` | `YqEFSZRo...` / `JFJeMiyA...` | session token (base64, ~43 B) — DIFFERS per session |
| `policies` | `[{"wifi":["192.168.1.103"]},{"wfd":["DESKTOP-PDPENKE"]}]` | transports: LAN ip list + WiFi-Direct device name |
| `wsPort` | 52212 / 52242 | PC WebSocket listen port (ephemeral) |
| `appVer` | `09.0.3.013.000` | app version string |
| `verCode` | 903013000 | numeric version |
| `os` | 6 | 6 = Windows |
| `scType` | 2 | Smart Connect type |
| `type` | 4 | device type |
| `i3` | 1 | flag |
| `serviceId` | 19 | service id (matches ServiceId enum) |

Outer `ips` (optional, top-level sibling of `wsData`): PC's reachable IPs, e.g.
`["192.168.1.103"]` or `["192.168.1.103","192.168.137.1"]` (.137.1 = WiFi-Direct/hotspot adapter).
qr4 omitted `ips` entirely; q1 had 1, q2/q3 had 2 — so the field is optional.

**Key facts proven:**
- `fp` differs between sessions → the self-signed cert (and thus pin) is **per-session**,
  transported out-of-band in the QR. No persistent pin store needed on the phone.
- `deviceId` identical across sessions → stable device identity.
- The phone validates the TLS pin against the QR `fp` before trusting the WebSocket.

## 9. mDNS discovery — exact wire schema (phone → PC)

The phone advertises service **`_readyformdns._tcp`** (note: *readyformdns*, not *readyfor*).
TXT records, exactly as on the wire (7 keys):

```
Cfp=7fd56fd3fcace58a39b25d30b873b76387cd554792eb745fe0cbb8e76e76fc13  <- HEX sha256 (64 hex)
cap=5365                                                                <- capability bitmask (0x14F5)
deviceName=moto g34 5G
type=1                                                                  <- 1 = phone
timestamp=1792059002416                                                 <- ms epoch
os=1                                                                    <- 1 = Android (PC = 6)
deviceId=ENqapqoQjidFwtpVg540tw==                                       <- base64
```

PC mDNS emits `fp` (**hex SHA-256**) + `appType`; phone mDNS emits `Cfp`
(**hex SHA-256**) without `appType`. The earlier PC-base64 statement was wrong:
the phone's mDNS pin validator compares hex. QR/endpoint pins remain base64.

## 10. LAN connection topology — EXACT (from pcap)

All LAN traffic after mDNS is **TLS 1.3** (certs encrypted; pinned via QR `fp` / mDNS `Cfp`).
Hosts: PC=`192.168.1.103`, phone=`192.168.1.100`.

```
CONTROL: phone:43552  <-> PC:52248    768 pkts  801 KB   phone Dials PC:wsPort (wss WebSocket)
MEDIA:   phone:50847  <-> PC:63950    865 pkts  1109 KB  PC Dials phone — stream #1 (largest = screen/video)
           phone:50847 <-> PC:63948    59 pkts  34 KB    PC Dials phone — aux #2
           phone:50847 <-> PC:63949    23 pkts  4.4 KB   PC Dials phone — aux #3
           phone:56597 <-> PC:61776    20 pkts  3.3 KB   aux #4
```

**Direction of connect (critical for the port):**
- **Control channel**: the PHONE initiates to the PC's `wsPort` (from the QR).
- **Media channels**: the PC initiates OUT to the phone — three separate TLS connections to
  a single phone media port (50847). That phone port is negotiated over the control WebSocket,
  NOT present in the QR.

**Not observed** (honest negatives): TCP 19527 (the static-analysis "pre-check" port) had no
LAN flow in this session; the PC did not advertise its own mDNS in this window (only the phone
did); no plaintext control/media payload (all TLS 1.3).

## 11. Live pairing handshake — phone → Linux peer (raw/pairing_session.txt)

First successful pairing against the prototype peer (phone scanned our QR), captured twice:

1. Phone → **TLS 1.3** client, **no client cert** (not mTLS at this stage); our cert accepted
   → QR `fp` pin confirmed end-to-end.
2. WebSocket request path is exactly **`GET /readyfor/`** (no subprotocol/auth header).
3. First frame: masked **text** frame, **1124 bytes of JSON** — saved to
   `capture/001_op1_1124B.{bin,txt}`. **Exact envelope** (matches the recovered Go struct
   `{SrcDeviceId json:"deviceId", SrcServiceId json:"srcServiceId", DstServiceId json:"serviceId",
   MsgId json:"msgId", Data json:"data"}`, plus a wire-only `senderId`):

   ```json
   {"data":{ "apkVerCode":9030130,"apkVerName":"09.0.3.013.0","cmd":0,"connType":0,
      "device":{ "cap":5365,"cloudPush":0,"deviceId":"Qeq6rLWsQMqgWSJA5+vNHg==",
        "deviceName":"moto g34 5G","i3":0,
        "minVerCode":{"1":7000000,"2":700000000,"3":7000000,"4":7000000},"os":1,
        "realDeviceId":"Icca7GgDY7+U16HiYxvNrg==","scType":1,
        "serviceVersions":{"0":3,"1":10,"2":5,"3":2,"5":1,"9":4,"13":1,"15":17,"16":6,"18":1,
                           "20":2,"21":3,"24":2,"25":2,"26":2,"32":2,"35":1,"36":1},
        "sku":"XT2363-5","sn":"ZA222L3QZQ","type":1,"verCode":9030130,"verName":"09.0.3.013.0"},
      "ip":"192.168.1.100",
      "ips":[{"connType":1,"fp":"x2fngiUKwgjrEtMjrPgQpYYeKdZhADbSLi46lL6OH9U=",
              "ip":"192.168.1.100","port":55053}],
      "limitService":0,"paired":0,
      "rfaServiceVersions":{"0":3,"1":10,"2":5,"9":3,"13":1,"15":17,"21":3,"32":2},
      "sdk":35,"serviceVersions":{ ...same keys as device.serviceVersions... },
      "token":"Z7c7VuHJDq61vzYtKfc3D9FhQ6dOeeyNfJ3pexcGnIY=","ver":3},
    "deviceId":"Qeq6rLWsQMqgWSJA5+vNHg==","msgId":76,"senderId":1,"serviceId":0,"srcServiceId":0}
   ```

   - `serviceId:0` = MANAGER, `srcServiceId:0`, `senderId:1`, **`msgId:76`**.
   - `data.device.cap=5365` (0x14F5) matches the phone's mDNS `cap`; `os=1`/`type=1`/`scType=1` (Android phone).
   - `data.device.deviceId` (`Qeq…`) differs from the pcap session's (`ENq…`) → per-session id;
     `realDeviceId` is the stable per-unit id.
   - `data.ips[0]` = the phone's **own** media endpoint (port 55053, `fp`=phone cert) — the PC may
     dial back here (see §10 media direction).
4. If the server stays silent: phone closes ~**5 s** later with **`3123: verify failure`**.
   The string is **not** in `winapi.dll` (only unrelated OpenSSL `Verify failure` in
   `libcrypto`/`dt2socket`), so **3123/"verify failure" is the phone's own** close reason:
   the phone waited for a server reply/verification and timed out.

### PC-side send/recv machinery (string-verified, functions located)

- Envelope/log: `Send to %s >>> msgId:%d, Src:%s(%d), Dst:%s(%d), data:%s, len:%d`;
  `OnServiceMsg, cmd: %d, size: %d, Data: %s`; `RecvMessage serviceId:%d, sid:%s`;
  `UnRecvMessage serviceId:%d`.
- Server-side setup logs: **`SetupServerWebsocket DeviceId:%s, Token:%s, Port:%d, isPending:%t`**
  and **`SetupServerWebsocket Discovery send error[%s]`** → the PC **sends a `DiscoveryMsg`**
  while setting up the server WS (candidate for the missing reply).
- `websocket.DiscoveryMsg` type exists (`framework/discovery`, `services/QrCode.BroadcastData`).
- Key functions: `websocketServer.readyforHandler` 0x1807b0000 (serves `GET /readyfor/`),
  `websocketServer.setup` 0x1807ae960, `websocketServer.run` 0x1807af3e0,
  `websocketServer.getInviteMsg` 0x1807b1980, `websocket.onMessage` 0x1807a6ba0,
  `websocket.handleMessage` 0x1807b3b40, `services/QrCode.onServiceMsg` 0x1808fc500,
  `services/CheckIn.onServiceMsg` 0x1807e11e0, `websocket.PaireDeviceWithPin` 0x1807a67c0.

## 12. Server reply on connect — EXACT schema (RECOVERED)

`websocketServer.readyforHandler` (0x1807b0000, serves `GET /readyfor/`) validates the incoming
`deviceId`+`token` and then **writes a JSON message straight back on the same connection** with
`gorilla/websocket.Conn.WriteJSON` (call site `0x1807b0d8b`):

- Read:  `Port:%d, ReadMessage: %s, remoteIp: %s` (0x180bc4d04) → `json.Unmarshal`.
- Valid: `Port:%d, DeviceId(%s) token(%s) is OK, IsPaired:%d remoteIp:%s, versionMap:%#v`
  (0x180be7751) — or `… is invalid …` (0x180bda6e3) / `… is expired …` (0x180bda71a).
- Build: `json.Marshal` (0x1807b0b76) of the payload struct, log `responseMsg:%s` / `Version: %s`
  (0x180b9af50), then `WriteJSON` (0x1807b0d8b); errors log `WriteJSON error:%s, remoteIp:%s`
  (0x180bb81e1).

The reply is a **`websocket.WebsocketMsg`** (type @ **0x180b4a460**, size 0x80) whose `data` is the
payload struct @ **0x180b4eae0** (size 0x80). Field names, offsets and json tags below come from the
binary's **reflect metadata** (`structField` entries; decoded directly from `winapi.dll`), not guesswork:

```
outer websocket.WebsocketMsg @ 0x180b4a460
  +0x00 SrcDeviceId   string        "deviceId,omitempty"
  +0x10 DstDeviceId   string        "-"
  +0x20 SrcServiceId  int           "srcServiceId"        <- always emitted, = 0
  +0x28 DstServiceId  int           "serviceId"           <- always emitted, = 0
  +0x30 MsgId         uint64        "msgId,omitempty"     <- 0 -> omitted
  +0x38 Result        int           "-"
  +0x40 LocalIp       string        "-"
  +0x50 RemoteIp      string        "-"
  +0x60 Data          []byte        "data,omitempty"
  +0x78 OnResult      func          "-"

data payload @ 0x180b4eae0
  +0x00 Version         int          "ver"                 <- literal 3
  +0x08 Cmd             int          "cmd"                 <- 0
  +0x10 Token           string       "token,omitempty"     <- literal "OK"
  +0x20 Paired          int          "paired,omitempty"
  +0x28 RemoteIp        string       "ip,omitempty"
  +0x38 ConnType        int          "connType,omitempty"
  +0x40 RemoteServers   []*Remote    "ips,omitempty"
  +0x58 ReadyforDevice  *Device      "device,omitempty"
  +0x60 ServiceVersions map[int]int  "serviceVersions,omitempty"
  +0x68 ApkVerCode      int          "apkVerCode,omitempty"
  +0x70 ApkVerName      string       "apkVerName,omitempty"

device.Device (data.device) @ 0x180b72ee0
  Id/RealId/Name/Type/Os/ScType/Sn/Sku("Sku")/Capability("cap")/Capabilities("caps")/
  Channel("ch")/Version("ver")/ServiceVersions/IsPaired("isPaired")/State/Online/Key("key")/
  VerCode/VerName/MinVerCode/CloudPush/FoldType/PairedVia

ips element @ 0x180b15ea0
  +0x00 ConnType "connType,omitempty"; +0x08 Ip "ip,omitempty";
  +0x18 Port "port,omitempty";       +0x20 Fingerprint "fp,omitempty"
```

Literal values assigned in the handler (assembly at `0x1807b09e0..0x1807b0d90`):

| field | value |
|---|---|
| `data.ver` | `3` (literal) |
| `data.token` | `"OK"` (literal @ 0x180b8eb64, len 2) |
| `data.paired` | paired flag from the handler |
| `data.connType` | `server.connType` (`[server+0xd8]`) |
| `data.serviceVersions` | a package-global `map[int]int` (0x1813cf080) |
| `data.device.deviceId` | `device.GetRandomDeviceId()` (0x18030d160) |
| `data.device.realDeviceId` | `device.GetDeviceId()` (0x18030da40) |
| `data.device.key` | pair key |
| `data.device.isPaired` | bool |
| `outer.deviceId` | `server.deviceId` |
| `outer.srcServiceId`, `outer.serviceId` | `0` |
| `outer.msgId` | `0` → omitted from JSON |

So the minimal wire reply is:

```json
{"deviceId":"<pc deviceId>","srcServiceId":0,"serviceId":0,
 "data":{"ver":3,"cmd":0,"token":"OK","paired":0,"connType":<n>,
   "device":{"deviceId":"<random>","realDeviceId":"<stable>","deviceName":"<pc>",
             "type":4,"os":6,"scType":2,"serviceVersions":{...},"isPaired":true},
   "serviceVersions":{...}}}
```

This is implemented in the prototype as `buildDiscoveryReply` (`reply.go`) and sent by default
(`rfproto serve`, `-reply auto`; `-replytoken` overrides `"OK"`; `-reply echo|'<json>'|''`
available). It is the direct answer to the phone's first message, and the best candidate for the
message whose absence causes the `3123: verify failure` close.

Also recovered (context): the PC **broadcasts a separate `Discovery`** on a timer via
`websocketServer.run` (0x1807af3e0) → `framework/discovery.Send` (0x1804591a0) whose WS channel
carries the **invite** built by `websocketServer.getInviteMsg` (0x1807b1980) — i.e. the same
`wsData` shape as the QR (§8). `readyforHandler` additionally calls
`websocket.verifyRemoteServers` (0x1807b4c20) and `websocket.setExcludeIp` (0x1807b4a20).

### 12.1 LIVE VALIDATION — the reply cleared `3123 verify failure` (2026-10-15)

Run: `./rfproto serve`, phone scanned the QR (`capture/001_op1_1124B.txt` is the full hello):

1. TLS pin **passed** (no client cert presented by the phone), WS upgrade `GET /readyfor/` OK.
2. Phone message #1 (msgId 78, 1124 B): manager hello with `paired:0`, `connType:0`, full
   `device{}`, `ips[0]` and **`data.token == our QR token`** — the QR token travels in-band
   (first captured run used a shortened hello, 76, as in the §12 header).
3. We sent the recovered reply. **No `3123` close — instead the phone kept talking**: within
   ~350 ms it sent 8 more frames (`capture/002..009`), then dropped with plain
   `1006 unexpected EOF` ~150 ms after the last (no close frame/audit this time).

Message 2 (msgId 79) is the **pair-confirm**, and the rest is service init:

| # | serviceId/src | confirms | content |
|---|---|---|---|
| 2 | 0/0 `data.cmd=1` | phone's **pair-confirm** | `paired:1`, `device{deviceId:realId, i3:1, scType:0, verCode:0, key:"9bUY…="}`, no `ips`/`token`. deviceId on the wire = 06T3… (= our ID!) |
| 3 | 1/1 `cmd=31` | QrCode | `view:7, port:0, reply:0, autoStart:false…` `ver:"1.0.0"` |
| 4 | 16/16 `cmd=14` | Input | `{inputMode:0, touchModeSupported:true}` |
| 5 | 9/9 | Clipboard | `{version:1}` `msgId:1` |
| 6 | 100/**204** `cmd=1018` | PC_WEBVIEW via TO_IOS | readiness report (`enabled/granted/unreadList ver:"5.0"`) |
| 7,8 | 25/25 `cmd=1` (twice, identical → i3:1) | DeviceStatus | full battery/wifi/bluetooth/screencap map `ver:"1"` |
| 9 | 25/25 `cmd=20` | DeviceStatus | `AiCapabilities` (pluginId 313 `com.motorola.mobiledesktop`) |

(Reference: Raw/ of the module — `services/DeviceStatus.onServiceMsg` 0x1807ec600 decompile,
`services/QrCode, services/pairing` reuse `SendServiceMsgByMap` 0x1807a3c80.)

Interpretation of the close: with no PC answer to its burst the mobile app gave up; it also
**ignores `state/online/caps/verCode/minVerCode/i3` (all omitted from its own broadcasts) and its
`deviceId` = its `realDeviceId`** — both had been filled by us from guesswork in the first run.

### 12.2 PC pair-ack — EXACT schema (RECOVERED, protocol-critical)

`websocket.onMessage` (0x1807a6ba0) — the module-level **dispatcher for incoming WebsocketMsg**
(refs: dispatch table built in `websocket.init.1` @ 0x180bf94e0, alongside
`onDiscoveryMsg 0x1807a61c0`, `onLocalServiceMsg 0x1807a5160`) — drives everything after the
hello; `readyforHandler` itself only opens the pipe. When a *manager* message with
`data.cmd=1` arrives (the pair-confirm above):

Branch sequence (0x1807a71a0: `cmp data.cmd,1` … `0x1807a7694`):

- `GetPairedDeviceKey()` (0x180308be0) → if key differs → set `IsPaired=true` on the DC record.
- `getWebsocket()` (0x1807b30c0) → `currentDevice.RemoveSavedNetworks … Allocated/in.a8` logic at
  0x1807a727d..0x1807a72bb (4/2→1/2/3 sends) — the "which connection-type am I" switch.
- Deletes GetDeviceSharedKey()/GenRandomKey() if still cached (0x18030d7a0/0x1802fe060).
- `…
  build reply = Copy(CurrentDevice())
  reply.device.key = <the INCOMING key>
  marshals on serviceId 0 with msgId=++globalMsgId (0x18141c838)
  calls SendServiceMsg(0) → WriteJSON
- `0x1807a7240` **serviceId 1 = same** (jump chain at 0x1807a71a5 vs 0x1807a7514 identical).

Recovered schema of the **pair-ack** payload:

```
outer WebsocketMsg:  deviceId = qrToken=="" ? "" : requestId      <- 907a72ea copies the REQUEST's deviceId
                     srcServiceId = 0, serviceId = 0
data (@0x180b4eae0, same struct as §12):
  ver     = 3            (only value ever written)
  cmd     = 1            <- 0x1807a7320 stores 3 (the enum value of "pair-ok") then 1/2:
           cmd=1 when PaireDevice(device,key) returned true
           cmd=2 when PaireDevice hit the paired-count cap       (both @0x1807a7333/0x1807a733f)
  device  = *device.CurrentDevice()   (0x18030dfe0)
            .key = incoming key         (0x18073db: [cur+0xa8] -> [new+0xa8]; copy via 0x180087d5c)
```

`PaireDevice` (0x180306fc0) does the persisting (mapRecord `SetConfig` @0x180304ee0) and only then
returns true → `cmd:1`; failed pairing (`rejected` / cap full) → `cmd:2`, and the mobile will
normally show "PC refused". `PaireDeviceWithPin` (0x1807a67c0) — the PC-initiated (PIN) flow —
ends in the same `SendServiceMsg(0)` with `{device, key}` in the payload.

So after our reply (§12), the phone receives:

```json
{"deviceId":"<pc id>","srcServiceId":0,"serviceId":0,
 "data":{"ver":3,"cmd":1,
         "device":{"deviceId":"<pc id>","deviceName":"<pc>","type":4,"os":6,…
                   "key":"<the key the phone just sent>","isPaired":true}}}
```

— i.e. **echo back the phone's own `device.key` inside our device object**, which is what makes
both sides converge on one shared 32-byte secret (and is exactly what §5 / §11 use for the
pairing-derived cryptographic connection: AES-256-GCM, key = base64‑decode(key)).

Implemented in the prototype as `buildPairAck` (`reply.go`), auto-fired on the pair-confirm:
`rfproto serve` (default `-pair auto`, `-paircmd 1`; `-pair off` / `-pair '<json>'` /
`-paircmd 2` to simulate rejection).

**Still open:** whether the phone needs *only* this reply, or also the periodic invite broadcast,
to clear `verify failure`. Next live test: `./rfproto serve -ip <lan-ip>` (reply is on by default)
and watch for a second frame from the phone (no close within 5 s).

### 12.3 BLE peripheral — the phone wants a Bluetooth peer (2026-10-17)

Live run 2026-10-17: mDNS + QR + WS all worked (hello matched our QR token, pair-confirm arrived,
our pair-ack went out), then the socket died **`1006`** ~85 ms after the 3rd service-init frame and
the app displayed **"turn on bluetooth on both devices"**. Phone Bluetooth was confirmed ON by the
user, and the Linux box's adapter is up (`74:70:FD:83:E0:9F`, BlueZ 5.86, `Roles=[central,peripheral]`).
So the missing half is the **PC-side BLE peripheral**, which the real PC always runs:

| winapi.dll | what it does |
|---|---|
| `discovery/ble.onBleState` 0x1806b7800 | whenever the adapter is powered → `ble.startAdvertise` |
| `discovery/ble.startAdvertise` 0x1806b7b80 | builds the advert payload (below) and calls the platform shim |
| `framework/platform.BleStartAdvertise` 0x18044afe0 | cgo `CBytes` → `ble_start_advertise` → `free` |
| `discovery/ble/negChannel.*` | foreground/background GATT negotiation channel, heartbeats; reacts to `onWsPairingStateChanged` |
| `services/WebView.allowBluetoothScan` 0x1808bd7c0 | broadcasts internal `ble_scan_turn_on` (1/0) |
| `discovery.stopScanWithDely.func1` 0x180457fa0 | re-enables BLE scan after a delayed stop |

`NativeAPI.dll` (sha256 `57df1b50…efcfc9`) is where the real work is: `BleServer.cpp` /
`BleClient.cpp` / `BleWatcher.cpp` / `Ble_iOS/ManufactureAdvScanner.cpp`. `ble_start_advertise`
(RVA 0x24cd0) reaches the coroutine at VA 0x180052780, which:

1. creates a `GattServiceProvider` with the service GUID **0x5246** (built from immediates:
   `+0x19e=0x5246`, `+0xcf4=0x10000000`, `+0x19f=0x80000080`, `+0xcfc=0xfb349b5f`),
2. creates the two characteristics (GUIDs at `0x18007fe70`=**0x0726**, `0x18007fe80`=**0x0722**;
   both also repeated at `0x18007fe90`/`0x18007fea8`),
3. copies the Go-supplied buffer (≤128 B) into a local `unsigned char[0x80]` and hands it to the
   advertising parameters (`winrt::array_view`, vtable `+0x60` on the params object), then calls
   `GattServiceProvider::StartAdvertising` (vtable `+0x58`) — logs `StartAdvertise done!`.

**Advert payload — recovered (not inferred)** from `ble.startAdvertise` (0x1806b7b80):

```
adv = [0x11] [cap & 0xff] [ base64decode(device.GetRandomDeviceId()) ... 16 B ]
       ^     ^
       |     +-- device.Device+0x58, json "cap"  (movzwl 0x1c0(%rsp) after the CurrentDevice() copy)
       +-------- fixed tag; makeFgAdvData (0x180452f60) uses [0x10,0x12,u16BE] instead
```

`device.GetRandomDeviceId` (0x18030d160) is the *same* id the PC publishes in mDNS TXT
`deviceId=` and in the QR `wsData.deviceId` — that is how the phone correlates the BLE advert with
the Wi-Fi peer it just paired (§8/§9). Note the first 2 bytes are **not** AD-structure length/type;
the whole blob is placed as the service-data payload for UUID 0x5246.

**Linux implementation** (`ble.go`, `rfproto serve -ble` / `rfproto ble adv|scan`, BlueZ via
`github.com/godbus/dbus/v5`):

* `org.bluez.LEAdvertisement1` — `Type=peripheral`, `ServiceUUIDs=[0x5246]`,
  `ServiceData={0x5246: <payload>}`, optional `LocalName`.
* `org.bluez.GattManager1` application — service `00005246-…` + the two characteristics below.

**Characteristic roles — RECOVERED (2026-10-17, no longer a guess).** `NativeAPI.dll` loads each
characteristic GUID and then sets `GattCharacteristicProperties` (a flag enum: Read=2,
WriteWithoutResponse=4, Write=8, Notify=16) immediately after it:

```
0x18005362a  movups xmm0,[0x18007fe80]   ; GUID 00000722-…
0x1800536d1  mov  WORD PTR [rax],0x6     ; -> Read | WriteWithoutResponse
0x1800540ae  movups xmm0,[0x18007fe70]   ; GUID 00000726-…
0x180054149  mov  WORD PTR [rax],0x8     ; -> Write
```

So the channel is asymmetric: **the peer reads `0x0722`** (`read`+`write-without-response`) and
**writes `0x0726`** (`write`). That matches the `negChannel` strings in winapi.dll —
`negChannel.OnDataIncoming deviceid : %s data : %s`, `OnDataIncoming unmarshal cmd failed with
error:%s`, `deviceId:%s, decryptData securityKey:%s`, `connect device:%X after %d ms` — a JSON object
with a `cmd`, carrying the same **WsData** type as the QR/mDNS.

**What travels on it — observed.** The phone's own logcat (2026-10-17, pid 7324) shows both halves.

Its write (the request half):

```json
{"cmd":0,"deviceId":"IXDUhJUYzqaLVA1Zl3wK5Q==","errorCode":0,
 "policies":[{"wifi":["192.168.1.103"]},{"wfd":["MYPC"]}],"reqId":1,
 "serviceId":19,"ver":1,"wsPort":0}
```

and the WsData it then holds (the answer half) — our QR's `deviceId`/`fp`/`token`/`wsPort`/`policies`
merged with the phone's own identity (`appVer`/`verCode`/`noncePhoneId`) and its struct defaults
(`cap:0` — the QR has no `cap`, which is how we can tell this object was assembled from QR fields):

```json
{"activeTrigger":false,"appVer":"09.0.3.013.000","authLevel":0,"cap":0,"cmd":1,
 "deviceId":"06T3cwaNbuA4xDLwylLvEw==","drawerMode":false,"dynamicPhone":false,
 "error":0,"expire":0,"fp":"c8U2bfap…=","i3":1,"noncePhoneId":"…","os":6,
 "policies":[{"wifi":["192.168.1.103"]},{"wfd":["MYPC"]}],"reqId":0,"scType":2,
 "sendingQueue":[],"serviceId":19,"sessionId":0,"source":"2","srcServiceId":19,
 "token":"…","type":4,"usedSource":0,"usingServiceSet":[],"ver":3,
 "verCode":903013000,"view":0,"wsPort":19527}
```

`wsPort:0` in the request is the point of the channel: "I do not know your WebSocket port yet."

**Historical Linux probe:** the original implementation exposed readable QR
JSON and inferred characteristic roles from Windows metadata. That did not
implement the Android client's framed request/indication exchange. It has been
replaced by authenticated binary endpoint negotiation; see §13.

**Live result (2026-10-17 15:57–15:59, `capture/c3`, `capture/c4`).** The phone *does* attempt the
channel — and it never reaches us. Every ~30 s its logcat shows:

```
sendGattForIpPolicy WsData{msgId=870, deviceId='HcvQqM1wLAxMnOBzd4vpOg==', serviceId=19, …}
 gattData:{"cmd":0,"deviceId":"HcvQqM1wLAxMnOBzd4vpOg==","errorCode":0,"reqId":1,"serviceId":19,"ver":1,"wsPort":0}
handleSendGattResult isSendSuccess:false, channel:3, errorCode:4, data:{…,"wsPort":0}, reason:
handleSendGattResult isSendSuccess:true,  channel:2, errorCode:0, data:{…,"policies":[…],"wsPort":0}
handleReceivedGatt channel:2, jsonStr:{…"wsPort":19527,"fp":"c8U2bfap…","token":"iahwxZkcea…"…}, isSecure:true
handleReceivedGatt setup ws mPcAllEnabled:true → create ws and as client → compare id ret:true
```

**Correction (2026-11-13):** connectivity channel `3` in this trace is
**Wi-Fi/mDNS**, not over-the-air BLE. Channel `1` is BLE. The successful channel-2
exchange is not evidence of a phone-local cached-record fallback. The absence of
BLE I/O therefore does not diagnose that channel-3 failure. The shared method
name `sendGattForIpPolicy` is not a transport identifier. The compact advertisement
was independently accepted by the Android parser, and the phone later connected
to the PC's actual BLE address. The missing authenticated endpoint protocol and
wrong GATT indication properties were implementation defects; see §13.

The rest of the BLE advertisement still stands: `RegisterAdvertisement` **and** `RegisterApplication`
both succeed, and `LEAdvertisingManager1.ActiveInstances = 1`. The adapter reports
`SupportedCapabilities.MaxAdvLen = 31` (legacy only), so the advert uses the **16-bit UUID form**
(16-bit service UUID + 16-bit service data) — Android resolves those to the full 128-bit base UUID,
and the total is 3 (flags) + 4 (uuid list) + 22 (service data) = **29 ≤ 31**. The 128-bit form needs
57 bytes and would be rejected; `-bleuuid128` allows an A/B if the controller ever negotiates extended
advertising.

### 12.4 device.Device identity values — EXACT (2026-10-17)

`device.CurrentDevice` (0x18030dfe0) assembles the struct the PC puts in every reply. It copies the
fields onto the stack starting at `rsp+0x28`, and that single function proves the offset mapping:
`Capability` is the only 16-bit store, and `VerCode`/`VerName`/`MinVerCode` land at
`+0xb0`/`+0xb8`/`+0xc8` from globals. The identity is therefore not guesswork:

| `Device` off | json | value | recovered from |
|---|---|---|---|
| `+0x30` | `type` | `4` | `movb $0x4,0x58(%rsp)` (0x18030e141) |
| `+0x32` | `scType` | `2` | `movb $0x2,0x5a(%rsp)` (0x18030e14f) |
| `+0x58` | `cap` | **`0x28ba` = 10426** | `movw $0x28ba,0x18141c4f4` in `device.init.0` (0x18030f24c) |
| `+0x60` | `caps` | **`map[int]int`** (13 entries) | global `0x1813cf088` (loaded at 0x18030e182) |
| `+0x80` | `serviceVersions` | **`map[int]int`** (20 entries) | global `0x1813cf080` (loaded at 0x18030e191) |
| `+0xb0` | `verCode` | `900001000` | global `0x1813505f8` |
| `+0xb8` | `verName` | `"09.0.0.001.000"` | global `0x1813a0400` (Go string) |
| `+0xc8` | `minVerCode` | `{1:7000000,2:700000000,3:7000000,4:7000000}` | global `0x1813cefd8`, built in `common/constants.init` (0x180188000) |

`cap` is a **bitmask**; `device.ResetCapByRegSetting` (0x18030f2c0) clears individual bits when the
matching registry switch is off (`RemoteDesktopMode` → bit 1, `Webcam` → bit 3, `RemotePCLock` → …),
so 0x28ba is the all-features default. The low byte (`0xba`) is what the BLE advert carries (§12.3).

`caps` and `serviceVersions` and `minVerCode` are the *same reflect kind* in the type descriptor,
i.e. all maps — `caps` is **not** a list. The exact maps, straight from the 20/13 map literals in
`device.init.0`:

```json
"caps":            {"1":2,"2":3,"3":2,"5":2,"9":2,"13":2,"15":2,"20":2,"22":1,"26":2,"204":2,"205":3,"206":3}
"serviceVersions": {"0":3,"1":10,"2":5,"3":3,"5":2,"9":4,"13":1,"15":16,"18":1,"20":4,"21":3,"22":2,
                    "24":2,"25":1,"26":1,"32":2,"36":1,"204":1,"205":2,"206":1}
```

Corrections this forced in the prototype (`reply.go`, `main.go`) — all previously wrong:

| field | was | now |
|---|---|---|
| `device.cap` | absent (→ 0) | `10426` |
| `device.caps` | `[0,1,2,…]` **array** | the 13-entry **map** above |
| `device.serviceVersions` | 8 entries, `15:17` | the 20-entry map above (`15:16`) |
| `device.verCode` / `data.apkVerCode` | `903013000` (the *phone's*) | `900001000` |
| `device.verName` / `data.apkVerName` | `"09.0.3.013.000"` | `"09.0.0.001.000"` |
| mDNS TXT `cap=` | `0` | `10426` (`-cap`) |
| mDNS TXT `type=` / `os=` | `1` / `1` | `4` / `6` (consistent with the QR §8 and the WS reply) |
| BLE advert byte 1 | `0x00` | `0xba` (low byte of `cap`) |

A JSON **type mismatch** (`caps` as an array where the phone expects a `Map<Integer,Integer>`) was the
prime suspect for the phone abandoning the session and returning to the scan screen, *before* the
phone's logcat was available. That hypothesis is now closed by §12.5: the reply parses cleanly, and the
actual cause is a missing `device.sn`. The schema corrections above are still correct and required.

`-cap` is now the single source of truth: mDNS TXT + WS reply + (low byte) BLE advert.

### 12.5 Root cause of "back to scan screen" — the phone CRASHES on a null `sn` (2026-10-17)

Earlier attempts compared frame counts and inferred a rejection (§11, runs of 9/8/4 frames). With USB
debugging enabled on the phone the answer is direct: `adb logcat` around the connect shows the app
**dies**, it does not time out.

```
15:14:49.599  MotoDesktopLog: [R4ConnectivityManager] CTEST handleWsAvailable ... r4Client:R4Client{
                ver=2, status='1', deviceId='06T3cwaNbuA4xDLwylLvEw==', realDeviceId='null',
                deviceName='MYPC', sn='null', minVerCode=null, serviceVersions=null, ...}
15:14:49.611  MotoDesktopLog: [R4ConnectivityManager] sendData ... client:RemoteClient{
                realDeviceId='06T3cwaNbuA4xDLwylLvEw==', deviceName='MYPC', cap=10100010111010',
                type=4', os=6', scType=2', i3=1', appVer='09.0.3.013.000', verCode=900001000'}
15:14:49.624  MotoDesktopLog: [DBManager] - insert RemoteDevice error!
                android.database.sqlite.SQLiteConstraintException:
                NOT NULL constraint failed: remote_device.sn (code 1299)
15:14:49.625  MotoDesktopLog: [DeviceConnectManager] - onDeviceOnline
                StationDevice{deviceId='06T3cwaNbuA4xDLwylLvEw==', sn='null', deviceName='MYPC', ...}
                findDevice = false.
15:14:49.628  MotoDesktopLog: [R4ConnectivityManager] handleWsMessage EXCHANGE_KEY
15:14:49.636  AndroidRuntime: FATAL EXCEPTION: pool-110-thread-1
                java.lang.NullPointerException: Attempt to invoke virtual method
                'boolean java.lang.String.equals(java.lang.Object)' on a null object reference
                  at com.motorola.mobiledesktop.sm2.A(SourceFile:82)
                  at com.motorola.mobiledesktop.sm2.j(SourceFile:22)
                  at com.motorola.mobiledesktop.service.s.c(SourceFile:6)
                  at com.motorola.readyfor.view.VersionMatchingDialogActivity.o(SourceFile:119)
                  at com.motorola.mobiledesktop.ez.onConnect(SourceFile:385)
15:14:49.712  ActivityManager: Process com.motorola.mobiledesktop (pid 27501) has died: fg TOP
15:14:49.778  ActivityManager: Start proc 6512 ... {com.motorola.readyfor.pairing.ui.AddDevicesActivity}
```

This resolves U5 and also re-reads the earlier frame counts: our reply **is** parsed and accepted
(`cap=10100010111010` is 10426 in binary, `type=4 os=6 scType=2 verCode=900001000` all as recovered);
the session ends because the app throws while persisting the device, and `AddDevicesActivity` — the
scan/pair screen — is restarted. A *more* correct reply fails *faster* precisely because the app gets
further into its commit path before hitting the missing field.

Two `device.Device` fields were absent from our reply, both populated by `device.CurrentDevice`:

| `Device` off | json tag | where the PC gets it | Linux counterpart |
|---|---|---|---|
| `+0x38` | `sn,omitempty` | `device.GetDeviceSn` (0x18030e280): `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`, cached in `0x1813d00f0/f8`, logged `deviceSn:%s` | `/etc/machine-id`, re-formatted 8-4-4-4-12 |
| `+0x48` | `Sku,omitempty` | `device.getDeviceHardwareInfo` (0x18030e9c0): `HKLM\HARDWARE\DESCRIPTION\System\BIOS\SystemProductName`, cached in `0x1813d0120/128`, logged `getDeviceHardwareInfo, deviceManufacturer:%s, deviceModel:%s, deviceSku:%s` | `/sys/class/dmi/id/product_name` |

`device.CurrentDevice` proves the wiring for `Sku`: it calls `getDeviceHardwareInfo` at 0x18030e0c9
only when the cached length is zero, then loads the two globals into `Device+0x48` (`mov
%rdx,0x70(%rsp)` / `mov %rax,0x78(%rsp)`; `0x70 = 0x28 + 0x48`). `getDeviceHardwareInfo` stores
`SystemProductName`'s result into exactly those globals (`mov %rbx,0x1813d0128` /
`mov %rax,0x1813d0120` at 0x18030eb9c/0x18030ebbf) — so `Sku` is the SMBIOS product name.

The prototype now sends both, overridable with `-sn` / `-sku` (`identity.go`), and logs them.

One naming wrinkle is kept as an explicit A/B rather than a guess: the tag in `winapi.dll` is
`Sku,omitempty` (capital S; the sibling `sku,omitempty` belongs to a different struct), while the
phone spells the same value `sku` in every frame it sends. `-skukey Sku|sku|both` selects the emitted
key, defaulting to the recovered tag.



---

## Status vs Route A/B goals

| Item | State |
|---|---|
| U2 KDF | EXACT — §1 |
| U2 Encrypt/Decrypt wire format | implementable precision — §2 (validate nonce/tag against 1 live sample) |
| U2 QR payload | **EXACT — §8** (prefix `motorolardpconnection` + JSON; field table above) |
| U2 server reply on connect | **EXACT schema — §12** (reflect-recovered types + literal assignments); implemented in `reply.go`; **live-validated §12.1** (cleared the `3123` close) |
| U2 PC `device.Device` identity values | **EXACT — §12.4** (`cap=0x28ba`, `caps`+`serviceVersions` maps, `verCode/verName`, `minVerCode`); implemented in `reply.go` |
| U2 PC pair-ack (manager `cmd=1`) | **EXACT schema — §12.2** (`{ver:3,cmd:1,device:{…,key:echoed}}` on `serviceId 0`); implemented as `buildPairAck` |
| U2 pairing JSON field lists | QR + server reply closed; other in-band msg fields still vocabulary-only |
| U1 BLE UUIDs | EXACT (0x5246 service, 0x0722/0x0726 characteristics) — §4 |
| U1 BLE advert | **EXACT payload — §12.3** (`[0x11][cap&0xff][base64decode(deviceId)]` as service data for 0x5246); Linux peripheral implemented in `ble.go`, BlueZ registry verified (`ActiveInstances=1`) |
| mDNS TXT | EXACT — §3 (PC, static) + §9 (phone, wire) |
| TLS pinning | EXACT scheme — §5; per-session pin confirmed — §8 |
| Connection topology | EXACT — §10 (control = phone→PC wsPort; media = PC→phone, 3× TLS) |
| Media framing/codec | OPEN — TLS 1.3 encrypted; needs decrypt (pin+key known) or endpoint capture |

Artifacts: `raw/gofuncs_winapi.json` (23,096 funcs), `ghidra-proj/RFA` (named DB),
`/tmp/opencode/decomp{,2}/` (140 decompiles), `msgcrypto.asm`, `msgdecrypt.asm`, `strrefs.py`,
`raw/qr_decoded.txt`, `raw/pcap_analysis.txt`.

## 13. Paired reconnect endpoint negotiation — corrected contract (2026-11-13)

The protocol-only specification was produced by isolated inspection of the
supplied Android 09.0.3.013.0 artifacts. No vendor implementation was copied.
Newer package equivalence is not assumed.

| Surface | Contract |
|---|---|
| PC mDNS TXT `fp` | Hex SHA-256 of TLS leaf DER |
| LAN negotiation | Binary WebSocket at `/readyfor/mdns`; no manager greeting |
| BLE primary | `0722`: write-with-response + indications; CCCD `02 00` |
| BLE alternate | `0726`: write-without-response + notifications; CCCD `01 00` |
| Control channel | `/readyfor/`; paired hello key is `data.device.key` |
| Endpoint/QR `fp` | Base64 of the same SHA-256 digest |

Both negotiation transports carry this paired binary frame:

| Offset | Content |
|---|---|
| 0–15 | Decoded 16-byte sender identity |
| 16 | `01` (authenticated encrypted negotiation) |
| 17–28 | Fresh random 12-byte nonce |
| 29 onward | AES-256-GCM ciphertext followed by the 16-byte authentication tag |

The AES key is the decoded saved pairing secret. **AAD is the first 17 bytes**.
The phone's current identity can rotate: the Linux peer authenticates against
saved keys and associates successful verification with the stored real ID.
Unauthenticated JSON identity alone never authorizes a response.

Request plaintext is a bare service-19 object with `cmd:0`, `deviceId` and `reqId`.
The reply is a bare object with `cmd:1`, the **PC's** identity, echoed `reqId`,
nonempty token/base64 fingerprint/policies, and the actual control `wsPort`.
The response binary header also carries the PC identity. There is no manager
envelope; `token:"OK"` belongs to the later control handshake instead.

Plaintext frame type `02` can cause a paired phone to delete its saved pairing;
it is not a compatibility fallback. The implementation rejects plaintext,
tampered headers, and unknown-key frames without sending an unpair message.

BLE `Value` is read-only to remote D-Bus callers. Local updates use `SetMust`,
with `EmitTrue`, so BlueZ receives the property change and emits the subscribed
indication. The old remote `Properties.Set` call could not update this read-only
property. Optional fragmented `0728` is not exported; ordinary frames must fit
the negotiated MTU. The observed encrypted endpoint reply was 408 bytes.

Evidence pointers: Android `ll9.java:52-65` (mDNS pin), `cv5.java:187-246`
(LAN endpoint/opcode), `mu4.java:111-170,242-284` (frame/AAD),
`kg7.java:36-57` (nested manager key), `ew4.java:348-364` and
`l30.java:510-575` (characteristics/subscriptions), `w1.java:475-504` and
`ud7.java:23-57` (connectivity channel/error mapping), all under
`phone-apk/jadx/sources/com/motorola/mobiledesktop/`.

Verification: targeted race-enabled tests cover authenticated LAN negotiation,
credential persistence and rejection, plus actual encrypted reply delivery over
a private D-Bus. On the physical phone, service-19 BLE send succeeded, the answer
was accepted as `channel:1, isSecure:true`, and Wi-Fi control connected after a
server restart without re-pairing. The user confirmed the connection.
