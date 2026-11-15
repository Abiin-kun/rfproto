# 00 — Overview: Lenovo Ready For / Smart Connect (Windows) architecture

Working notes for a clean-room Linux port. Every claim is tagged: **(O)** observation from
shipped bytes, **(I)** inference from observations, **(U)** unknown.
Evidence IDs refer to `/home/flame/Projects/ree/docs/INVESTIGATION.md` ledger; raw string dumps in
`/home/flame/Projects/ree/raw/`.

## 1. What the product is

"Ready For Assistant" ships as **Smart Connect** (UI title `Smart Connect`, exe `SmartConnect.exe`).
It pairs a Motorola/Lenovo phone or tablet with a PC over LAN/Wi-Fi-Direct/USB/BT and provides:
screen mirroring, app streaming, cross-control (PC keyboard/mouse driving the phone),
remote-PC (phone drives PC via RDP), file sharing, clipboard sync, webcam passthrough, and AI (MCP) hooks.

Lineage **(O)**: Go module path is `motorola.com/readyforassistant/*` (go-symbols in
`raw/gosym_*.txt`), PDB tree `apps_win_readyforassist_readyfor93_continuous` — Motorola "Ready For"
codebase now shipped by Lenovo (strings `Software\Classes\AppUserModelId\com.lenovo.readyfor`).

## 2. Build identity (REA Evidence)

All Go components share main module `motorola.com/readyforassistant v0.0.0-20260828032647-a32d20f3b398+dirty`,
go1.25.0, GOOS=windows GOARCH=amd64, `vcs.revision=a32d20f3b3988aaa41f75642ff4f998f9e7464a3` (2026-08-28).

| Binary | Go package | buildmode | Evidence ID | SHA-256 |
|---|---|---|---|---|
| `winapi.dll` | `libs/winapi` | c-shared | ev_b7514ecb…810f4b | fdd16d98…6bad1a3 |
| `RfaRpc.dll` | `libs/RfaRpc` | c-shared | ev_b39fc197…15fc5e | c714895b…cb6bce |
| `ScMcpServer.exe` | `plugins/ScMcpServer` | exe (-H windowsgui, -mguard=cf) | ev_146bca46…38a1df | da93a1e5…176399f |
| `launcher.exe` | `tools/Launcher` | exe | ev_089b7af0…ee00db1 | 8fdf15b8…732e09 |
| `Firewall.exe` | `tools/Firewall` | exe | ev_93769492…0089ef | 7bc1265f…4bb06f0b |

**(O)** `-buildmode=c-shared` for winapi/RfaRpc means they are DLLs exposing a C ABI to the MSVC
processes (`SmartConnect.exe`, plugins). CGO_ENABLED=1.

## 3. Dependency picture (what maps to Linux)

winapi.dll module text (Evidence above) lists the full stack. Directly reusable open-source deps:

- **Pion WebRTC v4** (`pion/webrtc/v4 4.2.13`, ice/v4, dtls/v3, sctp, srtp, rtp, rtcp, stun, turn, mdns)
  — video/datachannel transport is standard WebRTC ⇒ Linux-portable **(O/I)**.
- **`github.com/grandcat/zeroconf` + `miekg/dns`** — mDNS/DNS-SD LAN discovery ⇒ portable.
- **`github.com/xtaci/smux`** — stream multiplexing over a single TCP conn (session channel layer).
- **`github.com/gorilla/websocket`** — UI websocket.
- **`github.com/mark3labs/mcp-go`** — MCP (AI tool) server/client ⇒ ScMcpServer.
- **`github.com/atotto/clipboard`, `fsnotify`, `gopsutil`, `go-ole/wmi`** — host integration
  (the wmi/ole/gopsutil/WinAPI parts need Linux equivalents).
- **`lenovo.com/sdks/mesh v0.3.455`, `lenovo.com/sdks/udsmultimodal v0.3.103`** — private Lenovo
  Go SDKs (proprietary; **(U)** contents not visible in this artifact; likely cloud/multi-device).
- **CEF 144.0.12 + Chromium 144.0.7559.110** (FOSSA), Vue 3.5.17 + vue-router + element-plus 2.10.2
  UI ⇒ Linux-portable (CEF/Qt/Electron equivalent).
- **SDL** (SDL3.dll 2.8 MB shipped) — native video window path.
- **dokany 2.3.1.1000 + libnfs 5.0.2** (vendored zips in `third_party/`) — file sharing:
  Windows FUSE-equivalent (Dokan) + NFS client ⇒ Linux mapping is native FUSE + libnfs.
- **Detours, cppwinrt, devcon, CJsonObject, desktop-toasts** — Windows-only utilities.

Full third-party list: `raw/fossa_packages.txt` (151 packages), source `ThirdPartyNotices.txt`
(FOSSA report 2026-8-27, revision 9c7a8154…).

## 4. Process map (draft — pending IPC evidence)

**(O)** binaries and their embedded roles:

| Process | Language | Role (evidence: own strings/symbols) |
|---|---|---|
| `launcher.exe` (Go) | Go | `tools/Launcher`; launches/monitors SmartConnect; `rpc/client.runSmartConnect` |
| `SmartConnect.exe` (MSVC) | C++ | Main app: CEF UI host + `DiscoveryService` (initDiscoveryService/StartScan/StopScan), driver install (`InstallDriver--serviceId`), device message pump (`OnServiceMsg ... serviceId/msgId/data`) |
| `ReadyForService.exe` (MSVC) | C++ | Windows service "DaemonService": driver install, firewall, Bluetooth HCI capture (`BthHciCapture.cpp`), launches ScreenShare service, runs UI in user session (`RunReadyFor WTSQueryUserToken`) |
| `winapi.dll` (Go c-shared) | Go | Core framework: services/WebView (UI API), account, pairing, appstream, webcam, UniversalControl, Updater, clouddriver; Pion WebRTC; local HTTPS+WS server |
| `RfaRpc.dll` (Go c-shared) | Go | RPC/pipe + smux transport + device messaging |
| plugins (Go/MSVC exes/dlls) | mixed | ScreenShare, Vault (files), SmartAI, iOS, AppStream, ClipBoard, DragDrop, Webcam — declared in `plugins.json` |
| `ScMcpServer.exe` (Go) | Go | MCP server (AI tool interface), `plugins/ScMcpServer` |
| `Firewall.exe` (Go) | Go | Windows firewall rule helper (`tools/Firewall`) |

## 4b. Plugin ABI (native contract)

**(O)** from `objdump -p` export tables:
- MSVC plugins export **`Plugin_Start` / `Plugin_Stop`** (`AppStream.dll`, `ClipBoard.dll`,
  `DragDrop.dll`, `VirtualCameraPlugin.dll`) plus feature entry points, e.g.
  `R4DragDropService_{DragEnter,DragFiles,DragText,Drop,RegisterDragListener,UnRegisterDragListener}`
  and `UniversalControl{KeyInput,MouseButton,MouseEvent,TouchEvent,SetTouchPadTwoFingerDown,RequestSync}`.
- Go-built `SmartConnectCoreApp.dll` exports the PCM plugin vtable instead:
  `pcm_plugin_action`, `pcm_plugin_ininitialize`, `pcm_plugin_deininitialize`,
  `pcm_plugin_buffer_allocate`, `pcm_plugin_buffer_free`.
- Command-name convention `NEWPLUGIN_<PLUGIN>_<CMD>` (observed only in SmartConnectCoreApp):
  `GETCOMMANDLIST`, `GET_STATE`, `ON_STATE`, `REGISTER/UNREGISTER_MESSAGE`, `SEND/ON_MESSAGE`,
  `ON_MESSAGE_RESULT`, `REGISTER/UNREGISTER_BROADCAST`, `SEND/ON_BROADCAST`,
  `GET_PAIRED_DEVICE_LIST`.
- `SmartConnectExt` MSIX = sparse package registering a File Explorer context-menu COM handler
  (CLSID `917CF159-A4B5-41C8-8E8D-0E23A6605395`, "file add handler") — "send to device" shell verb;
  Windows-only, no Linux equivalent needed.
- `SmartConnect.exe` hosts CEF with `window.cefQuery` message router (`sendCefQuery` helpers,
  `WebcamCefV8Handler.cpp`) and OSR switches (`off-screen-rendering-enabled`,
  `shared-texture-enabled`, `external-begin-frame-enabled`) → the mirror window can render via
  CEF off-screen textures.

## 5. Frontend

**(O)** `html/index.html` loads `assets/index-Dg90TNrz.js` (16.5 MB Vite bundle, Vue 3 SPA);
`html/coreapp.html` is a second, older Vue 2/Element-UI window (card/float UI, 中文 strings);
`vaultPlugin/html/*` is a third Vue 2 UI for the file browser (element-ui 2.15.14).
