# 02 — Discovery & pairing

Source: string/symbol archaeology over shipped binaries (raw dumps in `/home/flame/Projects/ree/raw/`),
2026-10-15. Literals in backticks are observations; Go string blobs are length-sorted, so adjacency in a
dump ≠ co-use — binary-offset-verified facts are marked **[offset-verified]**. Tag legend in `00-overview.md`.

## 1. LAN discovery: mDNS/DNS-SD

- Service type **`_readyformdns._tcp.`** [wire-confirmed, pcap 2026-10-15; binary @0xba0f73].
  A second string `_readyfor._tcp.` also exists [binary @0xb9b9f2] (companion/alternate registration);
  enumerates `_services._dns-sd._udp.`. Companion mDNS name `smartconnect-pcp2p` [offset-verified @12171882].
- Implementation: `grandcat/zeroconf` + `miekg/dns` (build-info Evidence), with raw DNS building
  (`Construct mDNS packet`, `buffer not long enough to contain nonce`).
- Go packages: `framework/discovery`, `/discovery/{mdns,ble,usb,wifidirect,qrcode,message}`;
  mdns funcs `scan, startAdvertise, startScan, stopAdvertise, stopScan, getMdnsDevice, getMdnsIps,
  toText, txtToString, mdnsHandler`.
- Control verbs: `StartScan/StopScan/StartAdvertise/StopAdvertise/RestartAdvertise`.

### Advertisement payload

- **mDNS TXT record contains JSON**: `{"ver":"%s","type":"%d","sku":"%s","os":"%d"}` [offset-verified @12377162].
- Device record struct JSON tags (winapi.dll):
  `deviceId`, `scType`, `btAddr`, `isLite`, `usbIps`, `ips`, `wsPort`, `ip`, `acc`, `usb`, `wfd`,
  `invitePort`, `triggerWay` (all `omitempty` except `triggerWay`).
- Device-record log format [offset-verified @12500327]:
  `deviceId=%s, deviceName=%s, type=%d, os=%d, scType=%d, cap=%d, timestamp=%d, channel=%d isPaired=%t, state=%d, online=%t, foldType=%d`.
- Scan callbacks: `OnScanDevice random deviceId:%s, name:%s, entryStr:%s`, `find device:%X,id:%s,rid:%s,ios:%t,name:%s`,
  `StartAdvertise with a random deviceId:%s`, `registerService err:%s`, `mdns interface is null`,
  `mdns restart. mdnsIps:%s`.
- Parallel (non-mDNS) discovery channels: **BLE** (`BleConnection-%X-%d`, `BleStream-%X-%d`,
  `framework/discovery/ble/negChannel`), **USB** (`usbIps`, `accId`/`usbId`), **WiFi-Direct** (`wfd`).
- Events: `discovery_status_changed`, `ble_device_state_changed`.
- **Port 19527** is a fixed PC↔phone reachability pre-check: `connectTest19527: %s`, `%s:19527`,
  `pingPort(%d, %s) end: %v`.
- No PC↔PC or relay discovery exists — discovery is PC↔phone only.

## 2. Link setup (post-discovery)

`NativeAPI.dll` (C++/WinRT, imported from winapi.dll **by ordinal**) exports:
`ble_start_advertise/stop_advertise/start_scan/stop_scan/start_bg_scan/stop_bg_scan/connect/disconnect/send/read/pairing/is_paired`,
`wifidirect_start/stop`, `hotspot_get/set/start/stop`, `StartReadyForHotSpotThread`, `ClearAllReadyForHotSpot`,
`a2dp_open/close`.

- **Hotspot**: `Hotspot.cpp` → `Set ssid:%s, password:%s`; winapi generates credentials
  (`Failed to generate SSID and password: %v`; metric keys `su_htspt`/`fa_htspt`; `system_hotspot`;
  `Connect hotspot: %s`).
- **USB tethering**: `Usb.cpp` JSON `{"accId":%d,"usbId":"%ls","readMtu":%d,"writeMtu":%d,"state":%d}`;
  logs `CheckUSBTethering`, `USB TETHERING SUCCESS %s`, `usbSocket -> upgrade err`.
- **WiFi Direct**: adapter `Microsoft Wi-Fi Direct Virtual Adapter`; WinRT
  `WiFiDirect` AdvertisementPublisher/ConnectionListener.
- **Control channel after link-up**: WebSocket over TCP, optionally **smux-multiplexed**
  (`open smux session fail. addr:%s, err:%s`), plus `Port:%d, Renew Websocket`,
  `ready for websocket init`, `updateSecurityKey err:%s`; local control also via `\\.\pipe\ScRpcNamedPipe`.

## 3. Pairing flow

- Pairing API: `framework/websocket.PaireDeviceWithPin` (sic), `UnpaireDevice`, `onPairingStateChanged`.
- UI: `services/WebView.sendPinCode`, `CheckImageCode`, json tag `pinCode,omitempty`;
  web page `coreapp.html?action=pairing`; UI state `{"pair":"2"}`.
- **QR path**: `qrcode.GetInviteMsg`, `TriggerMsg`, `genRandomQrDeviceId`, `services.QrCode`,
  source `services/QrCode/web/QrCodeServer.go`; logs `TriggerStart deviceId:%s, GetInviteMsg:%s`,
  `TriggerStart rejected: device %s is not paired`, `RefreshQrCode refuse, qrCodeShow:%v, qrCodeVisible:%v`.
  Hotspot QR carries `MobileHotspot.QrCodeMsg` json `pwd,omitempty`.
- Events: `qrcode_show_changed`, `qrcode_visibility_changed`, `pairing_state_changed`;
  deep link `smartconnect://run?page=settings&index=home`.
- Pairing is **local-only**; Lenovo-ID auth gates only cloud features (see §5).

## 4. Transport SDKs (roles clarified)

| Binary | Role |
|---|---|
| `superlink20.dll` | WebRTC media engine; connection-facing exports `createConnection/closeConnection/createOffer/processSdp/addCandidate/addAudioTrack/addVideoTrack/addDataChannel/sendDataViaDataChannel/registerIceCandidateCallback/registerIceStateCallback/registerDataChannel*/getStats` |
| `RfaRpc.dll` | cgo bridge over winapi pub/sub: `rfa_rpc_start/stop/get_device_info/get_paired_device_info(_list)/get_service_port/get_websocket_info/forward/reverse_service_port/send/recv_message/send/recv_broadcast/un_recv_*/insert_event*/is_same_device_id/log` |
| `TapToXSDK.dll` | Tap-to-share sensor SDK: pipe `\\.\pipe\TapToX`, reg `SOFTWARE\Lenovo\TapToX\Runtime\Pipes`, exports `CreateTapToXSDK, init/startSensor/stopSensor/shutdown/setTapCallback/setStateCallback/sendPhoneTapEvent/queryVersion/forceChannel`, `isFujiMachine`, `isTapToShare`; talks to `TapToXService` |
| `SmartConnectIOTSDK.dll` | Third-party plugin SDK (`SC_CDP_SDK`/`IOTSDK`): pipe `\\.\pipe\SCIOTNamePipe`, 20 exports (`initialize/start/stop/getDeviceInfo/getPairedDeviceList/getServicePort/registerMsg/sendMsg/registerBroadcast/sendBroadcast/isServiceSupported/…`), imports `rfa_rpc_*` from RfaRpc.dll |
| `ludp.dll` | Telemetry/UDS only — no connection role |

## 5. Trust & crypto

- **`QuantumValidator.dll` is not crypto** — Lenovo entitlement check against
  `/cdn/installer/sc/checkMTCode` (also `/api/cdn/...`) with headers `X-Device-Id:`,
  `X-installer-Signature:`, `X-Installer-Nonce:` and query `?MTCode=&ramSize=&region=`
  (SMBIOS MT code/serial; log `QuantumValidator.log`).
- `Lenovo.CertificateValidation.Native.dll` — WinTrust helpers: `AddValidThumbPrints/IsCatSigned/
  IsCatalogSigned/ValidateAssembly/ValidateCertFile/ValidateXml*`.
- Go `common/lcv`: `ValidateAssembly`, `ValidateNamedPipeClient`/`ValidateNamedPipeServer`, `ValidatePid`
  (pipe peer vetting).
- **TLS**: Go `common/selfsign.SelfSign` issues per-machine self-signed certs
  (`Failed to create certificate: %v`); **fingerprint pinning** via
  `framework/websocket.verifyRemoteServers` + `common/utils.NewTlsConfigByFpBase64`
  (`verify remote server result: %t, ip: %s, connType: %d`, `fingerprint :%s, certificate fp:%s`,
  `VerifyConnection fp: %s, remote fp: %s`, `StartServerByPort: %d, fp: %s`).
- **PIN-keyed encryption**: `framework/discovery/message.{SetPin,IsMatchPin,getDeviceIdPin,pingToKey,Encrypt,Decrypt,tempKey}` —
  AES-GCM (`cipher.NewGCM`) plus `cipher.NewCBCDecrypter`, keyed from the pairing PIN.
- **Lenovo ID (cloud tier, PKCE)**: `https://passport.lenovo.com/v1.0/utility/lenovoid/oauth2/authorize?client_id=4d7271f5…&response_type=code&redirect_uri=smartconnect`,
  token `https://account.uds.lenovo.com/protocol/openid-connect/token`,
  grant `urn:ietf:params:oauth:grant-type:token-exchange`,
  profiles `https://smartconnect.lenovoid.motorola.com/ukiproxy/2.0/userprofiles`,
  broker `https://cloudbroker.uds.lenovo.com`; `services/account.*` (`genCodeVerifier/getToken/
  refreshToken/getLpsust/getUkiInfo/pairDevice/removeDevice/refreshDevices/getDeviceKey/signup/signin`).
  Gating string: `identityInfo is not auth` (cloud profiles/remote device list only).

## 6. Inferences

- Discovery = mDNS **`_readyformdns._tcp.`** [wire-confirmed] with a per-device TXT schema.
  **PC** TXT keys (static `mdns.toText`): `deviceId`, `deviceName`, `type`, `os`, `cap`, `timestamp`,
  `fp` (**hex SHA-256**, corrected by the phone's validator contract), `appType`. **Phone** TXT keys (wire, pcap): `Cfp` (hex sha256), `cap`, `deviceName`,
  `type` (1=phone), `timestamp` (ms), `os` (1=Android), `deviceId` (base64). Parallel BLE/USB/WiFi-Direct
  scanning; a random `deviceId` is used for unpaired advertise.
- Link setup selects a transport and refreshes the control endpoint through paired
  negotiation. The phone pins the self-signed PC certificate; observed control
  connections present no client certificate. See `05` §13 for reconnect framing.
- **Minimum viable Linux path**: mDNS advertise/scan with that TXT schema, TCP+WS with self-signed
  fingerprint-pinned TLS, PIN/QR pairing; smux optional.

## 7. Unknowns — most now closed (see `05-route-a-crypto.md`)

Closed by static RE + live capture:
- **QR payload encoding** → EXACT: literal prefix `motorolardpconnection` + JSON `{version, wsData{...}, ips?}`;
  full field table in `05-route-a-crypto.md` §8 (`raw/qr_decoded.txt`).
- **`pingToKey` KDF** → EXACT: b64(8× bswap32(pin)); AES-256-GCM discovery cipher wire format
  `[16B tag][1B type][12B nonce][ct]`, nil AD (`05` §1–§2).
- **BLE 128-bit UUIDs** → service `00005246-...` (0x5246), chars `0722`/`0726` (`05` §4).
- **mDNS TXT schema** → PC static keys + phone wire keys (`05` §3, §9).
- **Connection topology** → control = phone→PC wsPort (wss); media = PC→phone, 3× TLS to one phone
  media port (`05` §10, `raw/pcap_analysis.txt`).

Still open (each needs one more targeted capture, not more RE):
- BLE characteristic roles (write vs notify) and AD-structure framing of the adv payload — 1 `btmon` capture.
- Media stream framing/codec — encrypted (TLS 1.3); needs decrypt (pin+key now known) or endpoint capture.
- In-band (post-QR) pairing message field lists — vocabulary only; needs 1 decrypted control-WS sample.
- Whether `services/pairing` does network validation beyond `IsMatchPin`/`message.Encrypt`.
- Ordinal→name mapping of NativeAPI.dll imports (imported by ordinal; only export order available).
