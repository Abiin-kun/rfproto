# 03 — Screen mirror / app streaming / input injection

Source: string/source archaeology over shipped binaries (raw dumps in `/home/flame/Projects/ree/raw/`),
2026-10-15. Literals in backticks are observations from target bytes; tag legend in `00-overview.md`.

## 1. Component roles

| Component | Role (observed literals) |
|---|---|
| `SmartConnect.exe` | Main GUI, CEF 144 host (`ScCefClientManager`, `3rdLibs\cef_144.0.12…`), loads plugins from `plugins.json`, hosts `winapi.dll`; also `MotoRDPNamePipeServer.cpp` |
| `AppStream.dll` (`plugins\AppStream\`) | **App streaming, phone→PC**: `R4AppStreamingProxy/Service/Window`, `SuperlinkWrapper`, `videorender\OutputManager.cpp`, `Input\R4InputService.cpp`, `audio\R4AudioStreaming*` |
| `screenshare\ScreenSharePlugin.exe` (`plugins\ScreenSharePlugin\`) | **PC→tablet screen share** UI/service glue: `R4SS_VUE->W`, `screen_share_to_tablet_action`, HWND msgs `R4SS_P->S`, `CMD_SS_INITIALIZE_DRIVER`; creates/kills the helper (`SSSrv:succeeded in creating ss service`) |
| `screenshare\ScreenShareService.exe` | Privileged worker: driver control (`DriverHelper::InitDriver`, display/keyboard/pen/mouse/touch/audio) + WebRTC sender `SuperLinkHelper::SendVideoBuffer`, `sl_OnSDPCallback`; logs `%APPDATA%\ReadyFor\logs\superLinkLog_%s.log` |
| `screenshare\SDDriversInterface.dll` | Driver abstraction: `CVirtualDisplayDriver::ConnectDisplay`, `HidDeviceTouch/Pen/Keyboard::InputSend`, `SlDX11Device`, `STCcdApiDisplayConfig` |
| `ReadyForService.exe` | Service: driver install (`InstallCameraDriver`, `InstallMotoDriveDriver`, `InstallDokanDriver`, `InstallScreenShareDriver`), `RunScreenShareService`, touch↔display pairing (`MatchExtendTouch_A/_D`), session launch (`RunReadyFor WTSQueryUserToken`) |

## 2. Capture → encode → transport → render

- **PC capture is not DXGI Desktop Duplication / WGC** (no such strings). Frames come from the
  IddCx *indirect (virtual) display driver's* shared texture:
  `DisplayMonitorInstance::FrameBufferUpdate`, `pDevice1->OpenSharedResource1 failed!`,
  `AcquireSync error` [SDDriversInterface.dll].
- **Encode: Media Foundation MFTs inside `superlink20.dll`** — `MF.H264.EncoderSWMicrosoft`,
  `…HWAMD(mf_h264_vce)`, `…HWIntel(mf_h264_qsv)`, `…HWQCOM`, `mf_h264_nvenc` (+ `mf_h265_*`),
  `MFCreateMediaType`, `MF_MT_MPEG_SEQUENCE_HEADER`, `CODECAPI_AVEncCommonMeanBitRate`.
- **Decode: WebRTC + D3D11 HW decode** — `WebRTC-Use-MF/Enabled/`, `WebRTC-Use-D3D11DECODER/Enabled/`,
  `WebrtcD3D11VideoDecoder::Create`, `D3D11VA`, `RenderNV12DXGIMPO AcquireSync`.
- **App-stream render path**: D3D11 + DirectComposition per-app window —
  `WebrtcVideoRendererShaderImpl::InitD3D11`, `DCompositionCreateDevice2`,
  `CreateSwapChainForCompositionSurfaceHandle`, `videodump_%d_%d_NV12_%lld.yuv`, `d3d11 render fps`.
  (i.e. **not** a `<video>` element in CEF; CEF hosts only the shell UI.)
- **Codec params/gating**: `H264ParametersSet`, `H265ParametersSet`, `H265 disabled for vendorId %d, deviceId %d`.
- **SDL3 is audio-only** (`SDL_OpenAudioDeviceStream`, `SDL_PutAudioStreamData`).
- Pion's `video/VP8|VP9|AV1` + `simulcast` strings exist only in the Go signaling stack of
  `winapi.dll` — generic WebRTC plumbing, not the mirroring encoder.

## 3. Drivers (from .INF text + strings)

| Driver | Class / kind | What it is for |
|---|---|---|
| `lenovoDisplay` (`lenovoDisplayUmode1_0/1_2.dll`) | Display, UMDF via `IddCx0102`, HW `{63384FBF-…}\VID_LENOVO_INC_PID_LENOVO_VIRTUAL_DISPLAY_0003`, "Lenovo Graphics Adapter", `StaticDesktopReEncodeFrameCount=1` | **Indirect/virtual display** — screen share mirror+extend source |
| `lenovoDriverBus` | System KMDF, `Root\VID_LENOVO_INC_PID_LENOVO_VIRTUAL_BUS_0001`, "Lenovo Virtual Bus" | Parent bus for the virtual devices |
| `lenovoDriverHid` | UMDF2 HID via `mshidumdf`, "Lenovo Virtuals HID Device" | Virtual touch/pen/mouse injected **into the PC** (cross-control during screen share) |
| `lenovoDriverAudio` | WDM/KMDF (SYSVAD-derived), `VID_DATRONICSOFT&PID_SPACEDESK_VIRTUAL_AUDIO_RENDER/CAPTURE`, endpoints WaveSpeaker/SpeakerHeadphone/MicIn | Phone↔PC audio loopback |
| `LenovoKbfilter` | Keyboard-class KMDF, `Root\LenovoKbfilter` | Keyboard hook/filter (`IoctlHookWinPEventSubscribe`, `vKdbfilter_OnKeyboardHookAppCallback`) |
| `RFCamera` (+ `RFCameraMft0.dll` MFT `{BA31E4E4-…}`) | Camera avstream minidriver, "Smart Connect Camera" | **Webcam feature only** |
| `RFDriveFs2` | File-system driver (dokan-derived, `third_party\dokany_2.3.1.1000.zip`) | **Phone-storage mounting / file transfer only** |

Version marker: `LenovoSSDrivers.ini` → `SSDriverVersion=09/19/2025,1.5.000.142`.

## 4. Input injection (both directions)

- **Phone → PC** (screen share): input arrives on the SuperLink **WebRTC data channel** —
  `onDataChannel: msg= %d`, `sl_OnDataChannelMessageCallback`, `InsertTouchMoveEvent`,
  `InsertKeyUpEvent`, `DriverHelper::UpdateTouchEventInternal`, `Pen: nBX…nPressure…`,
  `Mouse nBX: %f…` — and is injected via virtual HID (`HidDeviceTouch/Pen/Keyboard::InputSend`,
  `KeyboardInputWrite`).
- **Touch ↔ virtual-display pairing**: `MatchExtendTouch`, `pcss:setRegMatchScreenTouch keyString=%s`,
  `RegOpenKey SOFTWARE\Microsoft\Wisp\Pen\Digimon`, `The pair is one between virtual display and HID`;
  SmartConnect forwards `%d %s send match touch to service. %d` (`DiscoveryService::OnGoMsg`).
- **PC → phone** (app streaming): AppStream `R4InputService` with
  `R4_INPUT_MSG_MOUSE_MOVE/KEYBOARD/MOUSE_BUTTON/TOUCH_EVENT`, `hook keyboard proc`,
  `CancelDownedKeysLocked`; and a security gate:
  `onServiceMsgReceived: Drop input packet from unauthenticated remote device`.
- Also present: `SendInput`/`mouse_event` in SmartConnect (`SimulateMouseClick SendInput failed: 0x%x`),
  `keybd_event` + Win+P hook (`InitWinpHook`, `DisableWinPAndPopCCList`) in ScreenSharePlugin.
- Go-layer feature gating: `device[%s][%s] does not support cross control`, `… does not support RDP`,
  `cross control is in use`, registry name `smartconnect_cross_control`.

## 5. Transport per feature

| Feature | Transport | Evidence literals |
|---|---|---|
| Screen share (PC→tablet) | WebRTC via `superlink20.dll` inside ScreenShareService; plugin↔service over HWND messages; plugin↔phone over app websocket | `SuperLinkHelper::InitSuperLink`, `Try to create peer connection`, `add video track`, `ProcessSDPOffer`, `AddCandiDate`, `SendVideoBuffer` ("background send 5fps to pad"), `kIceConnectionConnected`, `CMD_SS_*`, `R4SS_W->VUE`, `CMD_CONNECTION_READY` |
| App streaming (phone→PC) | WebRTC via `AppStream\SuperlinkWrapper.cpp`; legacy/non-WebRTC branch also present | `initSuperlink`, `webrtc_as_%lld.log`, `OnVideoDataCallbackSink`, `srtp://`, `R4_APP_STREAM_MSG_WEB_COMMAND/CANDIDATE`, `supportWebRTC`, `isWebRTC %d`, `protocol:%d, url:%s` |
| Webcam | Same superlink20 stack | `SuperlinkWrapper_WC`, `webrtc_wc_*.log`, `start webrtc failed with invaild sdp` |
| Remote PC (MotoRDP) | FreeRDP client started by Go service `services/MotoRDP.*` | `Start FreeRDP client:%s`, `Connect RDP to:%s, current MotoRDP status:%d`, `RDP_COMMAND_SYNC_DISPLAY_SETTINGS`, `remote doesn't support rdp over AS`, `freerdpWSocket`, `MotoRDP.ini` |
| Control/signaling | Pion WebRTC datachannels/tunnels in winapi.dll | `Initializing WebRTC API with default MediaEngine`, ICE/DTLS/SRTP/SCTP |
| RPC/tunnel channel | `smux` multiplexer in RfaRpc.dll (no media literals) | `*smux.Session`, `*smux.Stream`, `smux.writeRequest` |

**`ludp.dll` is not a media transport** — it is a telemetry/UDS client SDK
(`D:\work\vsProjects\ludp-windows-client-sdk-2.3`, SQLite tables `ludp_config/ludp_event/ludp_meta`).

## 6. Summary inferences

- Two distinct "mirror" directions: **PC→tablet** = ScreenShare stack + virtual display/HID/audio
  drivers over superlink20 WebRTC; **phone→PC** = AppStream (WebRTC receive, D3D11 render) or
  whole-phone via MotoRDP/FreeRDP.
- "Mirror vs extend" is a *display-configuration state* of the same IddCx virtual display
  (`It no need to set display orientation/scale for mirror mode` vs `set display scale for extend mode`),
  not two capture paths.
- Encoder chosen at runtime among MS SW / AMD VCE / Intel QSV / Qualcomm / NVIDIA NVENC; H265 can
  be disabled per GPU vendor.

## 7. Open questions (static strings cannot answer)

- Exact wire format of touch packets on the SuperLink data channel
  (`onDataChannel: msg= %d; %s`, `OnTouchDataSendCallback data:0x%p,size:%d` only).
- Whether PC-controlling cross-control (`smartconnect_cross_control`) rides the app-stream input
  channel, MotoRDP, or a separate path.
- Whether AppStream's non-WebRTC branch (`isWebRTC 0`, `srtp://` URL) is live or legacy.

## 8. Live post-pairing traffic — the media channels are announced in the WS session

Source: `capture/c1/` (2026-10-17 15:28 session; the first run that stayed connected, see
`raw/logcat_2026-10-17_success.txt` and `05` §12.5). The phone's whole service-init burst is now
captured — 83 frames, and ~761 KB pushed into the session while our peer answered only the hello
(`05` §12.1) and the pair-ack (`05` §12.2). Nothing else was needed for liveness: the connection held
for >20 s and the phone re-read the peer address once a second (`[DeviceConnectManager] Get [MYPC] R4
websocket ip: 192.168.1.103, current connectedIp: 192.168.1.103`).

Frame inventory (all frames are masked text JSON on the single WS connection):

| # | serviceId | cmd | bytes | what it is |
|---|---|---|---|---|
| 001 | 0 | 0 | 1123 | hello (handled; §12.1) |
| 002 | 0 | 1 | 529 | pair-confirm (`paired:1` + 32-byte `key`; handled; §12.2) |
| 003 | 1 (QRCODE) | 31 | 337 | QR/view state (`view:7`, `ver:"1.0.0"`) |
| 004 | 9 (CLIPBOARD) | — | 143/144 | clipboard availability, twice (`msgId` 5 and 10) |
| 005 | 16 (INPUT) | 14 | 201 | input service init |
| 006 | 25 (DEVICE_STATUS) | 20 | 513 | status query answer (`cmd=20`, no reply expected) |
| 007 | 25 | 1 | 608 | **full device status** (see below) |
| 008 | 204 → 100 | 1018 | 258 | PC_WEBVIEW unread counts, `unreadList` types 1/2 |
| 009–011 | 168 (FILES) | 1001/2005/2012 | 491/150/182 | **file-station channel announce** (see below) |
| 012 | 25 | 2 | **684193** | **wallpaper push** (see below) |
| 014/026/028/042 | 25 | 1 | 608 | status re-sent periodically |
| 015 | 32 | 1 | 202 | `{accessApps:0, crossDevice:false, port:0, ret:1, serverList:[], userId:0}` |
| 020 | 17 (APP_LIST) | 1 | **42385** | **app list + icon channel announce** (see below) |
| 035/075 | 37 | event | 3005/3224 | rich notification (`apk_icon` base64 JPEG, `actions[]`, `big_text`) |
| 036 | 5 (NTF) | event | 192 | status-bar notification key `0\|com.android.systemui\|1\|null\|10302` |
| 016–082 | 168 | 1001 | 491 ×55 | file station polled **once a second** (`[FileStationLog] sendMessage - cmd : 1001`) |

### 8.1 `serviceId 17` (APP_LIST) — the app list, and where the icon channel lives

`cmd 1` carries the phone's launchable-app list plus the endpoint the PC must use to fetch each app's
icon. 207 entries in the observed run; per entry `{cmp, pkg, name, iconName}`, e.g.
`{"cmp":"com.cloudflare.app.presentation.main.SplashActivity","iconName":"com.cloudflare.onedotonedotonedotone_com.cloudflare.app.presentation.main.SplashActivity.png","name":"1.1.1.1","pkg":"co…"}`.
The app list is filtered on the phone side (`[Utils] getFiltedAppList blackList:[com.lenovo.ota,
com.motorola.personalize, …]`).

```json
{"cmd":1,"ver":1,"apps":[…207…],"appLimit":5,"autoDownload":false,"wpApps":[],
 "fingerprint":"13edaa597d009d02dec52750247bdf11707927ddd882216168202fc5d0b5ad65",
 "port":54337,"token":"eYWVAn9CWeh2XATNzFSM2uLUKAXBb2bAkT9mJXvbytkZ"}
```

So the app list is not the icons: it hands over a **port + token** (and a fingerprint) on the phone.
This is the missing link for the capture-wide observation in `raw/pcap_analysis.txt` that "the PC
dials the phone's media port (3 TLS connections to one phone port)" — the PC opens a second,
separate (TLS) channel to fetch `iconName` blobs, authenticated by that token. `appLimit`/`wpApps`
are limits/whitelist hooks; `autoDownload` says whether the PC may pull icons unprompted.

**All three of those values are per-session (2026-10-17, `capture/c3` vs `capture/c4`).** Two
consecutive WS sessions a minute apart announce:

| session | `port` | `token` (first 12) | `fingerprint` (first 16) | apps |
|---|---|---|---|---|
| `c3/017` | 55218 | `o1SZo4wPTLK0` | `b8f12025198457d5` | 207 |
| `c4/017` | 58066 | `dbTfAktOsV1R` | `74ba9659b1f2ae35` | 207 |

so a fetch must use the endpoint from the announce it is answering, and the `fingerprint` (SHA-256
hex — as in mDNS, but unlike the base64 `fp` in the QR) is the pin for that endpoint's cert.

**And the port is not listening.** `192.168.1.100:55218` and `:54337` both answer
`connection refused` (no TLS either), and the phone's own socket table has no listener on either:
`adb shell cat /proc/net/tcp{,6}` shows only `192.168.1.100:64788`, `127.0.0.1:37529` and
`[::]:40001`. The icon endpoint is therefore opened **on demand** — which is exactly what
`services/appstream.downloadAppIcons` → `ForwardServicePort` → `tunnel.Forward` and the
`127.0.0.1:%d` literal describe (see §9). A peer cannot fetch icons by dialling the announced port;
it has to ask for the forward first.

### 8.2 `serviceId 168` (FILES) — file station announce and poll

`cmd 1001` carries a **JSON string inside the JSON** (`data.data`), i.e. a nested encoding:

```json
{"type":0,"path":"","port":0,"fp":"92BA4B9C…5775","fp2":"31c840c3…b89c",
 "deviceId":"Icca7GgDY7+U16HiYxvNrg==","capability":1,"timestamp":1792231107651,
 "nfs_version":8,"nfs_dependency":24,"dt2type":3}
```

`port` is the phone's file/NFS endpoint (0 here, i.e. "not started"), `fp`/`fp2` are SHA-256 hex
fingerprints (two, so two pins/certs — the phone's `realDeviceId` is the one signed here, not the
per-session WS id), and `nfs_version`/`nfs_dependency`/`dt2type` gate the protocol. `cmd 2005` is a
flush/ack (empty `data`), `cmd 2012` reports `{"MobileFileSyncStatus":false}`. The phone then re-sends
`cmd 1001` once a second (`[FileStationLog] sendMessage - cmd : 1001`), which is what makes the WS
traffic look busy — it is a poll, not media.

### 8.3 `serviceId 25` cmd 2 — the wallpaper is pushed as base64 PNG

```json
{"cmd":2,"ver":1,"md5":"736757db05c09546a88c1ef49e6a6ef7",
 "wallpaper":"iVBORw0KGgoAAAANSUhEUgAAAbMAAAMg…"}   // 684004 base64 chars
```

`iVBORw0KGgo` is the PNG signature, so bulk binary rides inline as base64 inside the same JSON
envelope, with an MD5 for cache de-dup. The companion `cmd 1` status frame gives the shell its state
(`wallpaperState:7`, `battery:11`, `wifi/data/bluetooth:true`, `wifiSignal:4`, `dataSignal:4`,
`screen:1`, `adb:true`, `usbReady:true`, `model:"moto g34 5G"`, `foldState/foldType`, plus the
phone's own `cap` map — which includes `202:2` and `204:17`, i.e. the PC-screen-share and webview
services, and a `27:0` entry the PC's map does not have).

**Consequence for the port:** any codec/mirror payload can be assumed to be carried the same way —
base64 inside the `data` of a normal `websocket.WebsocketMsg` on the service's own `serviceId` — and
the WebRTC/superlink stacks of §5 are only engaged *after* these announces, using the announced
`port`/`token`/`fingerprint` (app list, §8.1) or `port`/`fp`/`fp2` (files, §8.2).

## 9. The PC side of those channels — who consumes them, and over what transport

The Go half that consumes the announces above lives in `services/appstream` and
`services/FileTransfer`, and it does **not** connect to the announced port directly.

| winapi.dll | what it does |
|---|---|
| `services/appstream.init.0` 0x1808e4200 | registers the app-stream service (`SendServiceMsg(0x11=APP_LIST)`) |
| `services/appstream.onReadyForMsg` 0x1808e46a0 | handles the app-stream cmds; logs `OnServiceMsgForAppstream -> deviceId:%s, serviceId:%d, localIp:%s, remoteIp:%s, data:%s`, `OnServiceMsgForAppstream -> CMD_APP_STREAM_OPEN_LIST err: state: %d, pkg: %s`, `OnServiceMsgForAppstream -> CMD_APP_STREAM_SYNC_ICON success, deviceId: %s`; sends on **serviceId 0x11** (`mov $0x11,%ecx` before both `SendServiceMsg` and `SendServiceMsgByMap`) |
| `services/appstream.downloadAppIcons` 0x1808e6340 | the icon fetch: `downloadAppIcons->ForwardServicePort: realDeviceId: %s, forwardPort: %d, port: %d`, `downloadAppIcons->ForwardServicePort err: %s`, `downloadAppIcons->Download statusType: %d, reason: %s`, `downloadAppIcons->ReadDir err: %s` |
| `services/appstream.copyPngFile` 0x1808e6ec0 | writes each icon next to the app entry; `SetAppstream iconPath:%s, CheckImageCode fail:%s` |
| `services/appstream.loadApps` 0x1808e7720 | local cache (`.png`) |
| `services/FileTransfer.fileDownload` | `https://%s:%d/download?taskId=%s&token=%s`, `…/downloadFile?taskId=%s&token=%s`, and the same `ForwardServicePort` call |

**The icons are not fetched from the phone's port — they are fetched through a tunnel.**
`downloadAppIcons` calls `framework/connectivity.ForwardServicePort` (0x1806b3e40) →
`framework/connectivity/tunnel.Forward` (0x18043d1e0, logs
`TunnelForward remoteId:%s, %s[%d=>%d]`) which opens a local listener and pumps it over the
tunnel package (`framework/connectivity/tunnel.*`, 85 functions: `newTunnel`, `open`/`openAsClient`/
`openAsServer`, `sendCtrlMsg`/`handleCtrlMsg`, `streamConn.{readLoop,writeLoop}`,
`readMsg`/`writeMsg`/`frameReader`, `openTunnelStream`/`handleTunnelStream`/`startTunnelStream`,
`TunnelForward`/`SendMsg remoteId:%s, data:%s[%s]`, `tunnel(%s) not found`,
`listen response too late or too early`). Its transport is picked from two channels:
`tunnel.open` is called by `framework/connectivity/acc.OpenAsClient` and
`framework/connectivity/p2p.onConnected`, and `tunnel.SendMsg` by
`framework/connectivity.(*accChannel).Send` (which logs `AccWrite Error`, `onTunnelMsg:%s`,
`readyforRpc.read pipe:%s`) and `framework/connectivity/p2p.SendMessage` (logs `SendP2pMsg err:%s`,
`onConnected remote device id:%s`). So `acc` is the PC-local named pipe (RPC) and **`p2p` is the
phone link**, i.e. on Wi-Fi the tunnel rides the existing WebSocket as p2p frames.

Two confirmations from outside the binary:

* The port the phone announces is **not directly reachable**: after a live app-list frame advertised
  `port: 54337`, `192.168.1.100:54337` answered **connection refused** (and presented no TLS cert).
  It is therefore not a LAN-listening HTTP server; it is reached through the forward.
* The forwarded endpoint is local: `127.0.0.1:%d` is a literal in `appstream`'s string table, and
  icons land in a local directory as **`%s_%d.png`** (`blacklist`, `%([^%]+)`, `HOMEDRIVE` are its
  neighbours).

**Consequence for a Linux port (scope 2, app streaming):** answering the app-list announce is not
enough; the icons require the `p2p` framing plus the `tunnel` protocol (ctrl messages + stream
multiplexing). That is a self-contained, testable sub-project with a clear oracle: once implemented,
`downloadAppIcons`'s equivalent gets a PNG per app, and the phone logs
`OnServiceMsgForAppstream -> CMD_APP_STREAM_SYNC_ICON success`.

## 10. Screen share entry points — `serviceId 202` is confirmed, not inferred

`services/WebView.startScreenShare` (0x1808aae40) builds

```json
{"shareType":"%s","setAudio":true,"setScale":150,"setPosX":0,"setPosY":0,"deviceid":"%s"}
```

and sends it with `SendServiceMsgByMap`, where the service id is the immediate immediately before the
call:

```
1808ab06f  mov  $0xca,%ecx          ; 202 = PC_SCREENSHARE
1808ab077  call SendServiceMsgByMap  ; 0x1807a3c80
```

`0xca` is 202 — the same id the phone advertises in its `cap` map (`202:2`) in §8.3. The share types
are the literals `mirror` / `extend`, with `MIRROR` / `EXTEND` variants alongside `height`, `Webcam`,
`notifyaction`, `photos`, `sc_parlpsu` — i.e. mirror, extend (second display), and webcam-in are all
the same message family.

Other PC-side triggers, so the port can choose which to implement first:

| entry | notes |
|---|---|
| `services/WebView.startScreenShareFromMcpTool` 0x1808aa220 | AI/MCP tool → `startScreenShare`; logs `startScreenShareFromMcpTool current share type: %d`, `startScreenShareFromMcpTool, name:%s, realId: %s, cap: %d, ucCap: %d`, `Screen share is already in %s mode on device %s`, `Switch to %s mode requested for device %s` |
| `services/McpServer.(*ScMcpServer).handleStartScreenShare` 0x18085f200 | `handleStartScreenShare: toolName=%s, displayType=%s, deviceType=%s, deviceName=%s`, `displayType must be a string`, `unknown displayType: %s` |
| `services/McpServer.(*ScMcpServer).handleStartPCScreenShare` 0x180860020 | `handleStartExtendScreenShare: toolName=%s, deviceName=%s`, display types `Desktop`/`DESKTOP`/`VIRTUAL` |
| `common/deviceConfig.SetScreenShareType` 0x18041e4c0 | persists the mode (`device_config`) |
| `plugins\ScreenSharePlugin\ScreenSharePlugin.exe`, `screenshare\ScreenShareService.exe` | the native, driver-backed PC→tablet path (`CMD_SS_INITIALIZE_DRIVER`, `R4SS_*`); `AppStream.dll` is the phone→PC direction |

So the *control* half of screen mirror is a single service-202 message we can already send. What
follows it — the codec negotiation and the video path of §5 — is the large part, and on the real PC it
is handed to a separate native process rather than to winapi.dll.

### 10.1 The exact frame — recovered field by field

The format string is only the *inner* value. `SendServiceMsgByMap` (0x1807a3c80) is
`json.Marshal(map[string]any)` → `SendServiceMsg` → `websocket.SendMessage`, and the map at the 202
call site is filled with exactly three keys:

| key | key bytes | value |
|---|---|---|
| `ver` | 0x180b8eee9 | the 1-byte string `"1"` (`*(*string)0x180d160d0` → `0x180d126f0`, len 1) |
| `cmd` | 0x180b8eebf | the `int` **25** (`*(*int)0x180d15418`) |
| `data` | 0x180b8f553 | `fmt.Sprintf` of the 0x59-byte format string |

`json.Marshal` sorts map keys, so the payload on the wire is

```json
{"cmd":25,"data":"{\"shareType\":\"mirror\",\"setAudio\":true,\"setScale\":150,\"setPosX\":0,\"setPosY\":0,\"deviceid\":\"<phone>\"}","ver":"1"}
```

— **`data` is a JSON string, not an object.** The envelope is assembled by `websocket.SendMessage`
(0x1807a3f40):

* `msgId` = `++globalMsgId`, an atomic counter (`lock xadd %rcx,(%rdx)` at 0x1807a3ff7); never 0.
* `deviceId` = **the sender's own id**. An empty value is replaced by `device.GetDeviceId()` when
  `serviceId >= 99 || == 8` (0x1807a4016/0x1807a4020) and then normalised through
  `device.GetActualDeviceId` (0x1807a404b); `startScreenShare` passes `GetDeviceId()` explicitly at
  0x1808ab068. The phone is named only inside `data.deviceid` — the same string handed to
  `services/WebView.Connect(…, 202)` / `Disconnect(…, 0x1a)` (0x1808aaeaa / 0x1808aae85), i.e. the
  device the WebView service is opened for.
* `senderId` is declared without `omitempty`, so it is always sent; 1 is its value on every frame
  observed on this wire.

**Implemented** (`screenshare.go`, `serve -share mirror|extend`): the frame matches the above, and
`TestBuildScreenShareRequest` pins the payload shape, the inner string and the `cmd`/`ver` values.
The target id is learned from the phone's own frames (`data.device.deviceId`; `TestPeerDeviceID`) —
the envelope id is *not* usable for that, since the phone sets it to the sender on its service-init
frames but to the peer on the pair-confirm.

### 10.2 The first live attempt was malformed, so it settled nothing

Run at 16:08 (`capture/c5`) with `-share mirror`: the frame went out and the phone did **nothing** —
no `202` / screen-share / `MediaProjection` / RDP line anywhere in its logcat, and no reply frame. But
that frame was the pre-recovery one (object `data`, no `cmd`/`ver`, envelope id = the phone), so the
silence is not yet evidence about the control message. The `DashBoardActivity` that opens 4 s later is
not a reaction either: it opens on every connect (15:57:56, 15:59:03, 16:08:25).

Live verification of the follow-on is the open item: if the phone accepts the request, it starts its
screen-capture side and the session shows the §5 negotiation (or the phone logs a screen-share
state change); if the real PC always pairs this with the native plugin, the message alone will be
accepted but not acted on, which is itself the thing to record.

### 10.3 The corrected frame, sent alone, does nothing

Run at 16:20 (`capture/c6`, `-share mirror`), with the E25 frame sent 0.6 s after the pair-ack:

```json
{"deviceId":"06T3cwaNbuA4xDLwylLvEw==","srcServiceId":202,"serviceId":202,"msgId":1,"senderId":1,
 "data":{"cmd":25,"data":"{\"shareType\":\"mirror\",\"setAudio\":true,\"setScale\":150,\"setPosX\":0,\"setPosY\":0,\"deviceid\":\"HcvQqM1wLAxMnOBzd4vpOg==\"}","ver":"1"}}
```

The phone **ignored it**. It answered with its normal bring-up storm — frames #3–#27, services
`1`, `9`, `16`, `100`/`204`, `25`, `168`, `32`, `17` — and nothing else:

* no `serviceId 202` frame came back (nor any other reply to msgId 1);
* the 16:20:2x logcat has no `202`, screen-share, `MediaProjection`, `DisplayService`, or RDP line,
  and no capture-consent dialog or new display session;
* the only screen-adjacent lines are the per-connect policy read `isNearbyAppStreamingPolicyEnabled() 0`
  and `AppStreamHelper … handleFailure asRemoteClient is null` (16:20:37) — that is the app-stream /
  file path (§8.1, §9), not mirror.

So the 202 control message is **not a standalone trigger**: as §10 already inferred from the
binaries, the real PC pairs it with the native, driver-backed path (`plugins\ScreenSharePlugin\ScreenSharePlugin.exe`,
`screenshare\ScreenShareService.exe`).

One confound that looked plausible — a *preceding* WebView connect emitting extra frames — is now
**refuted**. `services/WebView.Connect` (0x1808b9fc0) is a dispatcher on `cl`, and the `cl == 0xca`
(202) branch only logs `Connect PC_SCREENSHARE: %s, displayView: %d` and calls
`services/WebView.updatePcssState` (0x1808b2fc0) → `framework/broadcast.SendBroadcast`, which is a
**local** broadcast and sends nothing on the wire. The `SendServiceMsg` (0x1807a3da0) calls on services
`0x21`/`1`/`0x63` belong to the `cl == 3` / `cl == 1` / `cl == 0x14` branches, not to 202; and
`services/WebView.startScreenShareFromMcpTool` (0x1808aa220) → `startScreenShare` sends no extra 202
frame either. So the **only** screen-mirror frame the PC emits is the 202 `cmd:25` message, and the
`capture/c6` frame was byte-faithful. What remains is not a missing frame:

1. **Timing.** We sent 0.6 s after the WS opened, mid bring-up (msgId 1 landed between the phone's own
   frames #3 and #4), whereas a user-driven mirror happens well after pairing settles. Now testable with
   `-sharedelay` / `-sharerepeat`.
2. **The target id.** `websocket.SendMessage` normalises ids through `device.GetActualDeviceId`
   (0x180306c40) before use, so `data.deviceid` may need the phone's *stable* `realDeviceId`
   (`Icca7GgDY7+U16HiYxvNrg==`) rather than the changeable envelope id we sent
   (`HcvQqM1wLAxMnOBzd4vpOg==`). Testable with `-shareid real`.

Same run, reaffirming §12.3 of `05` (E23): the phone's BLE send result was
`handleSendGattResult isSendSuccess:true, channel:2, errorCode:0`, and its `handleReceivedGatt …`
echoed **our current** `fp c8U2bfap…`, `wsPort 19527`, and QR `token` — the channel that works is the
QR-derived cache, not the BLE link.

### 10.4 Fidelity knobs for the next 202 attempt

Since the frame itself is byte-faithful, the remaining uncertainty is *when* it is sent and *which id*
it names. `serve -share …` exposes three knobs (all default to the faithful single-shot behaviour):

| flag | default | what it changes |
| --- | --- | --- |
| `-sharedelay <dur>` | `0` | wait this long after the pair-ack before sending the 202 frame (tests the timing hypothesis: we previously sent 0.6 s in, mid bring-up) |
| `-shareid device\|real` | `device` | which phone id goes in `data.deviceid`: the changeable envelope `deviceId`, or the stable `realDeviceId` (`Icca7GgDY7+U16HiYxvNrg==`) that the phone uses inside its own payloads (`device.GetActualDeviceId` normalisation precedent) |
| `-sharerepeat <n>` / `-shareinterval <dur>` | `1` / `3s` | send the request `n` times, `dur` apart (each resend gets a fresh `msgId`), in case a single send lands while the phone's 202 handler is still initialising |

Suggested A/B order: first `-share` with `-sharedelay 5s` (keep everything else faithful); then
`-shareid real`; then combine with `-sharerepeat 3 -shareinterval 2s`. Each run writes its own
`capture/c<N>/`, and the phone's `adb logcat` is the oracle — success looks like a `serviceId 202`
reply frame, a `MediaProjection`/`DisplayService` line, or a capture-consent dialog.

**Run 1 (the `-sharedelay 5s` A/B) is done — timing is ruled out.** Run at 13:59 (`capture/c7`,
`-share mirror -sharedelay 5s`): the frame went out 5.1 s after the pair-ack, once the phone had
already sent its DEVICE_STATUS/FILES/APP_LIST bring-up, and was **ignored exactly as before** — no 202
reply, no screen-share/`MediaProjection`/RDP/consent line (E27). The phone also emits no inbound-dispatch
line for it at all, so logcat cannot tell "received-and-dropped" from "handled-and-declined".

### 10.5 Why the 202 may be inert: the session never finishes the phone's handshake

The same run surfaced a bigger, previously-missed problem (E28): the phone's device session **never
completes the post-pair handshake**. From ~0.1 s after the pair-ack the phone logs

```
NFSManager - Receive nfs client status change [reaver] to: HAND_SHAKING
DeviceConnectManager - handleDeviceOnlineOnHandshake: cur version:8, cur capability:24   (~1 s apart)
DeviceConnectManager - current deviceName: reaver, device ip: 192.168.1.103, check canRead result: HAND_SHAKING
...
DeviceConnectManager - current deviceName: reaver, device ip: 192.168.1.103, check canRead result: HAND_SHAKE_FAILED   ← 13:59:49.225
```

`version:8`/`capability:24` are the phone's own handshake fields (`nfs_version:8`, `nfs_dependency:24`).
On `serviceId 168` (FILES) the phone repeatedly sends, and expects a reply to:

```json
{"type":0,"path":"","port":0,"fp":"…","fp2":"<phone cert sha256>","deviceId":"Icca7GgDY7+U16HiYxvNrg==",
 "capability":1,"timestamp":…,"nfs_version":8,"nfs_dependency":24,"dt2type":3}
```

Our prototype never answers, so it degrades to `HAND_SHAKE_FAILED` and restarts. The PC side of this
protocol is **not `winapi.dll`** — it is the vault/file-station plugin
`vaultPlugin\VaultPlugin.exe` (+ `libnfs.dll`, `dokan_nfs.dll`, `ShareHubServer.dll`, `dt2socket.dll`),
whose strings include `HandShakeOnlineDevice`, `serverInfo is null, failed to response client handshake
message, deviceId:`, `nfsport`, `nfsDataPort`, `nfs_capability` — a Dokan-backed NFS mount.

This matters for the mirror work because the phone ties *device-online* to that handshake
(`handleDeviceOnlineOnHandshake` is what sets the device online). Until the handshake completes, the
phone may not treat the device as ready and may drop later service commands — which would explain the
uniform silence in `capture/c5`–`c7`. Two plausible next steps follow (see §10.4 for the first): send
202 anyway once the handshake is answered, or test the phone's readiness gate directly.

### 10.6 The liveness + handshake probe set (implemented, awaiting a run)

Because "the phone received and dropped it" and "the phone handled and declined it" are
indistinguishable on the wire, the next run attacks the *gate* itself instead of the 202 alone. The
question (E28/E29, U9): **is the phone's feature-message dispatcher gated on readiness, or is 202
specifically inert?** The PC→phone traffic in the field is currently limited to the manager frames, so
we cannot tell whether feature messages are routed at all. Four cheap, independent probes go out in a
single session (`probe.go`); a reaction to *any* of them proves dispatch is live, while uniform silence
across several feature services backs the global-gate reading.

| flag | what it sends | schema confidence |
|------|---------------|-------------------|
| `-statusreply` | PC `DEVICE_STATUS` (serviceId 25, `cmd:1`) answering the phone's bring-up | **high** — key set, order and types are the phone's own status frame (`capture/c8/008`); values come from our recovered device maps |
| `-echosvc 25,9,16,100,32` | replays the phone's own frames back to it, re-addressed (our `deviceId`, fresh `msgId`, `senderId:1`) | **highest** — verbatim payload, no guessing |
| `-viewonline` | a PC_WEBVIEW (serviceId 100) frame after the pair-ack | low — only `cmd:1018` has been observed on 100, so this is a guess; `-echosvc 100` is the better test |
| `-fsreply auto` | answers the file-station handshake (serviceId 168 `cmd:1001`) with a best-effort serverInfo built from VaultPlugin's recovered keys (`nfsport`, `nfsDataPort`, `capability`, `nfs_capability`) | low — the real reply comes from the plugin and is backed by a running NFS server |

Notes on fidelity and method:

- `-statusreply` is the strongest *candidate fix* as well as a probe: the phone's bring-up order is
  DEVICE_STATUS (25) → FILES (168) → APP_LIST (17) → … and if the phone waits for a peer status before
  it will act on later service commands, answering this is exactly the missing step. It replies to any
  service-25 frame with a `cmd:1` status.
- `-echosvc` is schema-free by construction: the payload is the phone's, unchanged.
- `-fssvc` defaults to 168 (where the phone sends); 201 is the PC's `PLUGIN_FILE_STATION` sender
  (`services/WebView.SendFileStationMsg` → `SendServiceMsg(…, ecx=0xc9)`), so the channel can be A/B'd.
- Every probe is throttled to one send per ~0.9 s per kind, so the phone's `1 Hz` handshake retry loop
  does not turn into a flood, and all writes share the existing per-connection `writeMu`.

The run recipe (one session, captures wire + phone side):

```
# phone side, immediately before the run
adb logcat -c
# start serve
./rfproto serve -ip 192.168.1.103 -ble -capture capture -qr qr.png \
  -share mirror -sharedelay 5s -shareid real \
  -statusreply -echosvc 25,9,16,100,32 -fsreply auto -viewonline
# connect the phone, let it sit ~60 s, then dump the buffer BEFORE it wraps
adb logcat -d -v threadtime > /tmp/opencode/logcat_run5.txt
```

Then look for, in order of signal strength: (a) the phone's `check canRead` leaving `HAND_SHAKING`
(the handshake probe worked), (b) *any* phone-side log/UI change attributable to a feature service (the
dispatcher is live, and 202 is special), (c) a 202 reply once the handshake is answered (the gate was
the handshake), or (d) nothing at all (supports the global-gate reading, and the mirror path needs the
PC-side media stack rather than another control frame).

The logcat capture must be `-c` before / `-d` after: the long-lived `adb logcat` stream used for earlier
runs died (exit 255) and a mid-run re-clear silently dropped the window right after the 202 (E29).
**The ring buffer only holds ~5–7 minutes** (E36): the run-3 (`capture/c10`) oracle was lost because the
dump happened minutes after the session. Either dump immediately after the phone disconnects, or stream
for the whole run: `adb logcat -c && (adb logcat -v threadtime > /tmp/opencode/run.log &) ; ./rfproto serve …`.

## 11. Correction (E31–E35): the plugins are the protocol, and 202 runs the other way

Three artifacts found after the run-3 attempt re-scope the whole question. They are *shipped
observations* (a manifest, a string table, a decoded switch table), not inference.

### 11.1 The host is a **plugin broker**, and plugin ids share the phone's channel (E31)

`plugins.json` names every plugin with its `caps` and `bootServiceIds`:

| plugin | file | caps | bootServiceIds |
|---|---|---|---|
| ScreenSharePlugin | `screenshare/ScreenSharePlugin.exe` | `{"202":1}` | `[202]` |
| VaultPlugin | `vaultPlugin/VaultPlugin.exe` | `{"201":3,"168":3}` | `[201,168]` |
| SmartAIPlugin | `SmartAIPlugin.exe` | `{"31":3,"311":3,"313":3}` | `[31,311,313]` |
| iOSPlugin | `iOSPlugin/iOSPlugin.exe` | `{"204":1}` | `[204]` |
| AppStream | `AppStream.dll` | `{"15":3,"16":3,"21":3}` | — (in-process) |
| ClipBoard | `ClipBoard.dll` | `{"9":3}` | — (in-process) |
| DragDrop | `DragDrop.dll` | `{"24":3}` | — (in-process) |
| Webcam | `VirtualCameraPlugin.dll` | `{"3":3}` | — (in-process) |

The host (`winapi.dll`, Go) owns the WS server and routes a message by `serviceId` to **both** the phone
and the plugin that registered that id. So the `serviceId` on the wire is a *router key*, not a
"feature between phone and PC": some of the traffic we logged is host↔plugin RPC that the phone merely
overhears (and vice versa). This explains the standing puzzle that our prototype — a single process —
receives plugin-addressed traffic like `svc 17` and `svc 168`.

Worked example, the file station: the phone sends **168**; the host↔VaultPlugin control path is **201**
(`services/WebView.SendFileStationMsg` → `SendServiceMsg(ecx=0xc9)`, E30). Our `-fsreply` answered on 168
in a shape invented from the phone's own frame — but the object supposed to answer is `VaultPlugin.exe`,
whose strings show `HandShakeOnlineDevice--begin/end`, `serverInfo is null, failed to response client
handshake message`, `Handshake: NfsServer has not started, try to start NfsServer…`, `nfsport`,
`nfsDataPort`, `nfs_capability`, and a Dokan/NFS stack (`libnfs.dll` — an NFS *client* —, `dokan_nfs.dll`,
`ShareHubServer.dll`, `dt2socket.dll`). So the 168 handshake is a **file-station** feature backed by a
real NFS server, orthogonal to mirroring; the phone's 1 s re-announce is that channel doing its own
thing, and its failure is not evidence of a global readiness gate.

### 11.2 `serviceId 202` is PC-screen → phone, and its cmd enum is now complete (E32)

`ScreenSharePlugin.exe` is native C++ and its 202 dispatcher is a clean MSVC switch: `cmp ebx,0xca`
(202), `cmd-2` range-checked to `0x46`, byte-index table at RVA `0x13ac8`, jump table at `0x13a64`.
Decoding it with the log strings beside each target yields the whole protocol:

| cmd | name / effect (from the adjacent log literal) |
|---|---|
| 5 | `CMD_SL_OFFER` — WebRTC SDP offer from the peer |
| 7 | `CMD_ANDROID_DISPLAY_ROTATION` |
| 12 | `CMD_ICE_FAILURE` — Superlink start failure |
| 13 | "screenshare running is error" |
| 16 | pad-call reconnect (stop, then start) |
| 25 | **`CMD_VUE_SCREEN_SHARE_START`** — "Request to start ss from vue" |
| 26 | `CMD_VUE_SCREENSHARE_STOP` |
| 27 | `CMD_VUE_SET_AUDIO` |
| 28 | `CMD_VUE_SET_SCALE` — "vue set windows scaling" |
| 29/32/35/64 | `SET_RESOLUTION` — "trigger windows display settings" |
| 30/31/60/62 | set OSK status |
| 34 | move display → `setPosX`/`setPosY` |
| 50 | **`CMD_ANDROID_SCREENSHARE_START`** — "Request to start ss from tablet" |
| 52 | `CMD_ANDROID_SCREENSHARE_STOP-S` |
| 54 | `CMD_ANDROID_SET_AUDIO` |
| 56 | `CMD_ANDROID_SET_SCALE` |
| 66 | "ss start fail, reason: %s" |
| 68 | "android device locked, user reject ss" |
| 72 | `CMD_ANDROID_RESTART` |

Everything in this table is about **the Windows display**: the `CMD_VUE_*` family scales, re-positions
and re-resolutions the *PC's* desktop under remote control, and `drivers/screenshare` ships a virtual
display driver plus HID and audio drivers. The transport is **SuperLink — i.e. WebRTC** — from
`superlink20.dll` (`CMD_SL_OFFER`/`CMD_SL_CANDIDATE`/`CMD_ICE_FAILURE`).

Therefore the frame we have been sending faithfully (`{"cmd":25,"data":"{\"shareType\":\"mirror\",…}","ver":"1"}`,
E25) asks the PC's ScreenSharePlugin to start sharing **the PC's** screen to the phone. The phone has no
reason to open a `MediaProjection` capture for it, and it does not. **Every negative 202 result
(E26/E27/E29) and the run-3 silence are explained without any readiness gate.** The 202 direction was
simply inverted, and `202` should be dropped from the phone→PC mirror scope — it belongs to the
"PC screen to phone / phone as second display" feature.

### 11.3 The phone→PC screen is **AppStream**, and it is WebRTC (E33)

`AppStream.dll` (native, owns `{15,16,21}`) *is* the phone-screen-on-PC client:

- per-device streaming window — `Create R4AppStreamingProxy for device %s`, `StartStreaming deviceId %s, package %s`, `HandleStreamingStateChanged session id %lld, state %d`;
- D3D11 **NV12** video renderer — `WebrtcVideoRendererShaderImpl::InitD3D11 / CreateStagingTextureNV12 / ReleaseSwapChainTexture`;
- audio — `Create R4AudioStreamingProxy for device %s`, `setup webrtc audio stream!`;
- input injection (PC→phone) — `R4InputService`, `InputMsg`, `CancelAllTouchInputs`, touchpad and wheel handlers, `ShowTouchToast`, "drop input packet from unauthenticated remote device";
- **WebRTC signalling over the same WS** — `ProcessSdp`, `AddCandidate`, `onWebRTCAnswered`, `onWebRTCCandidateCallback`, `R4AppStreamWebRTCMsg`, `enter onAnswerCallback peerId:%lld, sessionId:%lld`;
- bootstrap descriptor from the phone — `invalid stream info, session id %lld, key %s, isWebRTC %d, webRTCData empty: %d`, `HandleWsMessageReceived R4SocketInfo is NULL`.

The host side: `services/WebView.SendOpenAppstream` → `SendServiceMsg(ecx=0xf)` = **service 15**; the
inbound dispatcher is `services/appstream.onReadyForMsg` (log `OnServiceMsgForAppstream -> deviceId:%s, serviceId:%d, …`),
switching on `cmd` ∈ {1, 3, 9, 10} plus `CMD_APP_STREAM_OPEN_LIST` and `100 CMD_APP_STREAM_SYNC_ICON`;
`services/appstream.downloadAppIcons` is the icon fetch (tunnel-mediated, E21/E22).

Note what the phone already does in our captures: it **pushes the app list and the icons by itself**
(`svc 17 cmd 1` with `port`/`token`/`fingerprint`, and `svc 37` icon payloads, E34). So the phone side of
app-streaming is already speaking to us; what is missing is a PC that can be the WebRTC **receiver**.

### 11.4 The notification channel already works, and `c1` is our richest session (E34/E35)

`capture/c1` (801 frames, first and longest session) shows `svc 5` (~69 frames, the event/index channel:
`{"type":0,"event":2,"key":"0|com.android.systemui|1|null|10302","version":"1.0"}`) and `svc 37` (~47
frames, the payload channel: app-icon base64 JPEG, rich notifications with `actions`/`big_text`, an 11 KB
messaging notification with `conversation_title`/`message_persons`/`messages`, a 34 KB media notification
with `big_picture`). Both are **push** channels — they need no PC→phone trigger, and they demonstrate
that a non-manager feature service *does* flow.

`c1` is also a pre-pair session (`data.paired:0`, no `data.device.key`) versus `c10`
(`data.paired:1`, with `data.device.key`), and it still pushed notifications.
New frame shapes visible only in `c1`: `svc 168 cmd 2008` `{"data":"false"}`, `svc 9` `{"data":{"version":1},"msgId":1}`,
`svc 32 cmd 1` `{"accessApps":0,"crossDevice":false,"port":0,"ret":1,"serverList":[],"userId":0}`,
`svc 16 cmd 14` `{"data":{"inputMode":0,"touchModeSupported":true},"instanceId":0,"messageId":0}`,
`svc 25 cmd 22` `{"mUntrusted":false}`.

### 11.5 What this changes

- **Drop `202` from the phone→PC scope.** It is the reverse feature (PC desktop → phone), already fully
  mapped in §11.2.
- **Phone→PC screen/apps = `AppStream`, services 15/16/21, WebRTC + input injection** (§11.3). This is a
  genuinely larger build than a control frame: a WebRTC endpoint (offer/answer/ICE), an H.264/VPx decoder
  and renderer, plus the input channel back to the phone.
- **Notification mirroring (svc 5 + 37) is already reachable** and needs no PC→phone trigger; it is the
  cheapest feature to complete (decode `apk_icon`/`big_picture` base64 and render them).
- **The file station (`168` + `201`) is VaultPlugin's**, NFS/Dokan-backed; it is a separate sub-project
  (§11.1).
- Before further live probing, fix the oracle: **dump logcat immediately** (E36).

### 11.6 The cast control plane, read out of the APK (E37/E38)

The pulled APK settles the "how do I make the phone share its screen" question that the PC binaries
could not, because the initiator in this direction is the **phone's own plugin framework**
(`com.moto.smartconnect.plugin`), not a Windows process. The chain is plain Java, unobfuscated, and
short enough to state exactly.

**The request.** `com.moto.smartconnect.plugin.bean.CastPhoneReqData` is a Gson bean with
`{cmd, ver, width, height, fps}` and this enum:

| constant | value | meaning |
|---|---|---|
| `CMD_CAST_MIRROR` | `0` | mirror the phone screen |
| `CMD_CAST_DESKTOP` | `1` | desktop mode |
| `CMD_CAST_MOBILEUI` | `2` | phone UI in a window |
| `CMD_CAST_STOP` | `100` | stop |

**The carrier.** `com.motorola.smartconnect.connectivity.services.a` extends `SCBaseServiceHelper` and its
constructor passes `0x3e9` to the base class; `preHandleMessage` then rejects every `WsData` whose
`serviceId` is not `1001`. Its `cmd` switch (packed-switch keys `1..17` and `19..25`) maps key **`0xb` (11)**
to `FeaturesServiceHelper.H(deviceId, req)`, which logs

```
castPhone, castCmd: <req>, deviceId: <id>
```

and, for `cmd 0/1/2`, calls `ReadyForService.M(cmd, MotoDisplayMode(width, height, fps), deviceId)`.
`MotoDisplayMode` is the parcelable the core module hands to the virtual-display service
(`width, height, fps`), so the three numbers in the bean are the capture parameters, not a UI size.
`cmd 100` calls `ReadyForService.C(deviceId)`.

**The listener gate.** The same switch has two control commands:

| outer cmd | effect |
|---|---|
| `1000` (`0x3e8`) | add `WsData.srcServiceId` to the helper's listener `HashSet` |
| `1001` (`0x3e9`) | remove it (`a.W`) |

When the phone pushes `CastPhoneStateData` it builds `WsData(deviceId, srcServiceId=1001, …)` and then
iterates that set calling `setServiceId(listenerId)`. So **the phone addresses its state replies to a
service id the PC chooses and registers with cmd 1000**; without the registration there is nothing to
observe. The identical pattern carries `AppStreamListData{apps, wpApps, cmd, resultCode, ver}` with
`srcServiceId=4`, which is why the app-stream list is a *phone push* rather than a reply.

**The wire frame** (Gson over `WsData`, serialised by `pm5.h0` = plain `Gson.toJson`, so keys are exactly
the field names):

```json
{"deviceId":"<pc>","srcServiceId":15,"serviceId":1001,"msgId":1,"senderId":1,"ver":"1.0",
 "data":{"deviceId":"<pc>","cmd":11,"ver":"1.0",
         "data":{"cmd":0,"ver":"1.0","width":1920,"height":1080,"fps":30}}}
```

`FeaturesServiceRequest` is `{deviceId, cmd, ver, data}`; its `deviceId` is what `H` uses to look the
client up, so it must be the id the phone stored for us (our hello sends the same value as `deviceId` and
`realDeviceId`, so either spelling works — `-castdevice real` exists to A/B it).

**Implemented.** `serve -cast mirror|desktop|mobileui|stop`:

```
serve -cast mirror                 # registers listener 15 (cmd 1000), then sends cmd 11 / inner cmd 0
serve -cast mirror -castdry        # print the exact frame and exit
serve -cast stop                   # cmd 11 / inner cmd 100, then unregister (cmd 1001)
```

with `-castw/-casth/-castfps` (default 1920×1080@30), `-castlisten` (default 15), `-castregister`,
`-castdelay`, `-castrepeat/-castinterval`, `-castdevice device|real`, `-castsvc`/`-castcmd` for A/B.
`cast_test.go` pins the frame shape.

**Run recipe** (the oracle is the point):

```
adb logcat -c
./rfproto serve -cast mirror ...      # first cast attempt
adb logcat -d > /tmp/opencode/logcat_cast1.txt
grep -E 'castPhone|FeaturesServiceHelper|SCBaseServiceHelper|ConnectStateChanged|AppStream' /tmp/opencode/logcat_cast1.txt
```

Success criteria, in order: `SCBaseServiceHelper preHandleMessage wsData:…` proves our frame was routed to
1001; `FeaturesServiceHelper castPhone, castCmd:…` proves `H` ran; `onConnectStateChanged:` (from `mh3`)
and an inbound `serviceId 15` frame carrying `CastPhoneStateData` prove the virtual display came up and
the listener registration worked. If only the first appears, the `deviceId` lookup failed; if the first two
appear but no state, the display start was refused (check `MotoDesktopService`/`MediaProjection` lines).

**What is still open.** The control plane starts a phone virtual display; it does not by itself say which
socket carries the pixels. `AppStream.dll` (services 15/16/21) is the PC-side WebRTC receiver and
`CastPhoneStateData` carries `connectState`, so the likely sequence is: cast request on 1001 → state on the
listener id → SDP/ICE on 15/21 → media. That is the next thing the logcat run should show.




### 11.7 The missing PC→phone broadcast: service-25 DEVICE_STATUS carries the cap map (E39–E41)

Before any of the 1001 work can be seen in the UI, the phone has to believe we *have* features. That is not
in the QR, the mDNS TXT record, or the hello's `caps` map — it is a separate runtime message.

**Gate chain (read from the APK):**

```
tc7.i(device,z)                     dashboard tile list
 └─ b54.I(device,n) = J(n) && K(device,n)
     ├─ J(n)  phone-side: w46.U(serviceId) + product flags
     └─ K(device,n) → w46.Y → w46.X(device,serviceId)
         ├─ service.s.e(serviceId, os, type, realDeviceId)
         │    └─ reads dr7.i  (paired-device DB, written from the hello's serviceVersions)
         └─ service.g.L(serviceId, realDeviceId)
              └─ reads g.q  (realDeviceId → Map<serviceId,version>)
                   └─ written ONLY by DeviceStatusServiceHelper.handleRemoteMsg
                        when serviceId 25, data.cmd == 1, data.cap = {…}
                        (persisted as SharedPreferences "sp_latest_device_cap")
```

Confirmed on a live capture: the phone sends its own status early (`serviceId 25, cmd 1, cap
{"1":3,"2":3,"3":1,…}`), and the PC is expected to answer with its own. Our server had that answer behind
an opt-in probe (`-statusreply`, default **off**), so `g.q` was always empty and the device page rendered
zero tiles — "no features being broadcast".

**Tile ↔ service map** (`j4.a()`):

| tile | serviceId |
|---|---|
| App streaming | 15 |
| Mobile desktop / Screen share | 1 |
| Webcam | 3 |
| Hotspot / tethering | 13 |
| PC lock | 22 |
| Cross control | 26 |
| Smart clipboard | 9 |
| File explorer | 20 |
| File station | 168 |
| Screenshare W2A | 202 |

**Corrected status contract (2026-11-13).** `serve` answers status bring-up by
default with service 25, `cmd:1`, the PC capability map, and **`senderId:0`**.
The selector is essential: `1` routes to Ready For Assist instead of the
mobile-desktop capability/battery receiver. Physical verification showed seven
tiles after correcting that selector, without changing advertised capability bits.

**Reconnect correction (2026-11-13).** Connectivity channel `1` is BLE and channel
`3` is Wi-Fi/mDNS; the old “channel 3 is Bluetooth” diagnosis was wrong. Both carry
the same authenticated binary service-19 endpoint negotiation. BLE `0722` needs
write-with-response and **indications**, not readable QR JSON or notify-only
properties. `ble.go` now shares the AES-GCM endpoint codec with `/readyfor/mdns`;
the phone accepted the encrypted BLE answer and subsequently connected over
Wi-Fi. See `05` §13. Optional fragmented `0728` is not advertised.

**Run recipe for both fixes:**

```
adb logcat -c
./rfproto serve -ble -cast mirror -castdelay 5s -inject 127.0.0.1:19528 -capture capture/c14
# on the phone: tap Connect on the PC card (no QR scan)
adb logcat -d > /tmp/opencode/logcat_status1.txt
grep -E 'initRunInExecutorService|execute\(\) complete|lazyInit\(\) failed|saveLatestCap|handleRemoteMsg|addConnectivityListener|notifyMessage serviceId|castPhone' /tmp/opencode/logcat_status1.txt
```

Expected, in order: `initRunInExecutorService()` → `execute() complete`; `addConnectivityListener serviceId
= 1001` (or its absence is explained by `lazyInit() failed`); `DeviceStatusServiceHelper … getCap()` /
`saveLatestCap()` after our status frame; then, with `-cast`, `notifyMessage serviceId:1001,
listener:<FeaturesServiceHelper>` and `castPhone, castCmd: 0`.

### 11.8 Status subsystem selection — live tile visibility fix

Ordinary mobile-desktop status uses the envelope
`{deviceId:<PC>,srcServiceId:25,serviceId:25,senderId:0,msgId:<counter>,data:<object>}`.
The nested object has `cmd:1`, `ver:"1"`, and `cap` as a service-ID/integer map.
The cache identity comes from the authenticated connection's real device ID.

`senderId` is a subsystem selector, **not** a PC-versus-phone label. Value `1`
selects Ready For Assist; a parseable status sent there can coexist with an empty
mobile-desktop capability cache and battery -1. The prior `senderIDPC` assumption
has been removed; the constants now name mobile-desktop and Ready For Assist.
Other probe routes were not silently changed.

Static protocol references: Android `zc7.java:1020-1057` and matching smali
`zc7.smali:1548-1646` (subsystem split), `service/g.java:461-480,852-925`
(capability/battery receiver), under `phone-apk/jadx/sources/com/motorola/mobiledesktop/`
and `phone-apk/smali/classes/com/motorola/mobiledesktop/`.
Ordinary tile visibility checks capability-key presence; changing values 2 to 3
is not a dispatch fix.

Verification: the status wire regression checks selector 0. On the connected
physical PC card, App streaming, Mobile desktop, Screen share, Webcam, Hotspot,
PC lock, and Display and Control became visible, and battery changed from -1 to
the published value. Tile visibility is not implementation of those features.

### 11.9 Native producer receive path — physical verification

The independent SDL3/Pion receiver was exercised with a moto g34 5G:

- The authenticated phone supplied 215 catalog entries. Clock and Files streamed
  VP8 video at 360 × 800. Native pointer navigation and text entry changed the
  actual phone app UI.
- Service 1 admitted mobile desktop, followed by service-15 video at 1280 × 720.
  The desktop app drawer accepted pointer input and search text.
- Service 21 negotiated a separate Opus stream at 48 kHz stereo. A locally
  generated 440 Hz WAV played inside both modes was recovered at the PC output
  monitor. Phone audio state 2 means paused; state 3 resumed output. Idle or
  stopped source snapshots do not establish an audio transport failure.
- Closing the sessions removed their phone virtual displays and the native
  audio output stream. The authenticated control connection remained usable.
  Reverse microphone requests were declined without stopping phone-output audio.

**Directional routing correction.** The phone's catalog and feature replies
arrived with envelope `senderId:1`. The PC's outbound `senderId:0` requirement
does not make that an invalid inbound sender. The receive path now dispatches
only the recognized feature services on the authenticated connection without
that erroneous sender filter.

**Native memory correction.** Two reopening runs aborted with
`malloc(): corrupted top size` during `native_video_push`. The 360-pixel image
used a tight RGBA row even though the installed FFmpeg SSSE3 converter writes
whole 16-pixel blocks. The receiver now allocates via
`av_image_alloc(..., AV_PIX_FMT_RGBA, 64)` and retains the returned pitch for SDL
texture upload. No authentication or memory protections were weakened.

`TestNativeVP8PortraitReopen` exercises three portrait decode/close generations
using an independently generated VP8 fixture. A throwaway guard-page program
decoded 60 moving portrait frames in each of three decoder generations with
pitch 1472, no access past the output buffer, and matching SDL last-pixel
readback. The user manually verified the fixed reopening behavior as well.

Targeted race-enabled tests cover native decoding/input, phone audio lifecycle,
catalog routing, display reuse, and cleanup. Physical tests used SDL X11;
native Wayland windowing/input were verified separately under nested KWin.
Long-duration stability, other phones, and physical touch hardware were not
verified by these runs.

