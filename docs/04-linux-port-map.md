# 04 — Linux port map (draft)

Mapping Windows components → Linux equivalents for a clean-room implementation.
Status legend: **[ready]** established from evidence; **[tbd]** awaiting protocol evidence
(discovery/pairing + IPC reports pending).

## Runtime / UI layer

| Windows | Linux |
|---|---|
| CEF 144 + Win32 shell (`SmartConnect.exe`, `ScCefClientManager`) | Independent SDL3 workspace; no vendor SPA or browser runtime |
| `cefQuery` message router bridge | Direct calls into the Go authenticated-session hub |
| OSR shared-texture render (`off-screen-rendering-enabled`, `shared-texture-enabled`) | FFmpeg video decode and SDL textures on the main OS thread |
| Go c-shared DLLs (`winapi.dll`, `RfaRpc.dll`) called from MSVC | Plain Go shared libs or — better — fold into one Go daemon process (the C ABI was only needed for the CEF host) |
| Named pipe `\\.\pipe\ScRpcNamedPipe` | Unix domain socket (Go `net` handles both; smux works over any stream) |
| Windows service (`ReadyForService.exe`, SCM, `WTSQueryUserToken`) | systemd unit (+ `machinectl`/polkit if a user-session UI is needed) |
| Plugin `Plugin_Start`/`Plugin_Stop` DLL exports / `pcm_plugin_*` vtable | Same contract via Go plugins or subprocess+socket; keep `plugins.json` (already portable JSON) |
| Firewall helper (`Firewall.exe`, Windows Firewall COM) | `firewalld`/`nftables` rules, or nothing (document required ports) |

### Current phone-to-Linux workspace

`serve` now uses a native SDL3 window, not the earlier loopback HTTP/browser UI.
Pion terminates WebRTC; FFmpeg and Opus provide native video/audio decoding.
FreeType renders the independent UI. `-ui=false` keeps serving without a window;
the native development/runtime libraries are still build/runtime dependencies.

The existing authenticated hub remains the single owner of phone resources:
service 1 admits mobile desktop (`view:2`), service 15 launches/signals video,
service 16 carries input, service 17 supplies the app catalog, and service 21
negotiates shared phone audio. These routes use subsystem selector `senderId:0`.
Service 1001's cast probe controls the phone's receiving side, not this producer.
Closing the UI releases held input and stops its video/desktop/audio sessions.

Physical moto g34 5G verification now covers app video/input/audio/teardown and
mobile-desktop video/input/audio/teardown. The live video codec was VP8; audio was
Opus. Physical tests used X11; native Wayland rendering/input were checked
separately under nested KWin. This is not a claim of all-device compatibility.
Earlier TLS-media observations below concern other/legacy paths; they are not
the transport chosen for the current service-15 WebRTC receiver.


## Transport / protocol

| Windows | Linux |
|---|---|
| Pion WebRTC v4 (winapi.dll) | **Unchanged** — pure Go, cross-platform |
| `xtaci/smux` session mux | **Unchanged** |
| `grandcat/zeroconf` + `miekg/dns` mDNS discovery | **Unchanged** |
| FreeRDP client (MotoRDP) | **Unchanged** (FreeRDP is cross-platform); PC-side RDP *server* needs xrdp-style integration |
| `superlink20.dll` (proprietary WebRTC media engine + MF encoders) | **Replace** with pion/webrtc + FFmpeg/GStreamer/VAAPI/NVENC encode, or WebRTC native |
| `ludp.dll` telemetry SDK | **Drop** (analytics, not required) |
| `TapToXSDK.dll` (tap-to-share sensor, pipe `\\.\pipe\TapToX`) | Optional: needs a hardware-accelerometer tap equivalent — **drop for MVP** |
| `SmartConnectIOTSDK.dll` (third-party plugin SDK over `rfa_rpc_*`) | Portable contract — it just wraps the RfaRpc C ABI (same Go code, rebuild for linux/amd64) |
| mDNS discovery `_readyformdns._tcp` [wire-confirmed] + TXT schema | **Unchanged** (grandcat/zeroconf is cross-platform) |
| BLE / USB-tether / WiFi-Direct setup (NativeAPI.dll, WinRT) | **Replace**: BLE → `bluez` (tinygo dbus or a BLE lib); USB accessory → `usbmuxd`-style USB IP/adb; WiFi-Direct → document as unsupported or use NetworkManager |
| Fixed reachability port TCP 19527 | **Verify** — in static analysis but not observed in live session; likely optional |

## Video pipeline (PC→tablet screen share)

| Windows | Linux |
|---|---|
| IddCx **indirect display** driver + shared texture capture | Virtual output: **virtual DRM connector** (`vkms`/`virtual_drm`), **Xorg dummy**, **Xephyr**, or **PipeWire/portal** capture (Wayland: `wlr-screencopy` / xdg-desktop-portal) |
| MF MFT encoders (NVENC/QSV/VCE/SW) | FFmpeg `h264_nvenc`/`h264_vaapi`/`h264_qsv`/`libx264` — same vendor-availability gating |
| D3D11 decode (`WebrtcD3D11VideoDecoder`, D3D11VA) | FFmpeg/VAAPI/VDPAU decode → texture upload |
| D3D11 + DirectComposition swapchain render per app window | SDL3 renderer and per-session video textures |
| SDL3 audio stream | **Unchanged** (SDL3 is cross-platform) |

## Input injection

| Windows | Linux |
|---|---|
| Virtual HID: `lenovoDriverHid` (UMDF2 HID, touch/pen/mouse) + `LenovoKbfilter` | **uinput** virtual device (`/dev/uinput`) — no kernel module needed |
| Touch↔virtual-display pairing (`MatchExtendTouch`, `SOFTWARE\Microsoft\Wisp\Pen\Digimon`) | Map touch device to output via compositor config (wlroots output-mapping / Xorg input mapping) |
| PC→phone input (AppStream `R4InputService`) | SDL keyboard/pointer/touch events → service-16 JSON, targeting the phone's returned display ID |
| `SendInput`/`mouse_event`/`keybd_event` simulation | uinput or `ydotool`-style injection |
| Virtual audio (SYSVAD-derived `lenovoDriverAudio`) | **snd-aloop** (ALSA loopback) or PipeWire virtual sink/source |

## Webcam passthrough

| Windows | Linux |
|---|---|
| `RFCamera.sys` (avstream minidriver) + `RFCameraMft0.dll` device MFT | **v4l2loopback** (`/dev/videoN`, `modprobe v4l2loopback`) |

## File sharing / clipboard

| Windows | Linux |
|---|---|
| Dokan (`RFDriveFs2.sys`, dokan2.dll, dokanfuse2.dll) + `libnfs` (vaultPlugin) | Native **FUSE** filesystem + libnfs (already C, portable) |
| `ClipBoard.dll` (Windows clipboard) | `github.com/atotto/clipboard` already supports X11/Wayland |
| `DragDrop.dll` (OLE drag-drop) | UI-level DnD in the webview |

## Security / trust

Established (see `02-discovery-pairing.md` §5):

| Windows | Linux |
|---|---|
| Go `common/selfsign.SelfSign` — per-machine self-signed TLS cert | **Unchanged** (crypto/x509 cross-platform) |
| `framework/websocket.verifyRemoteServers` + `NewTlsConfigByFpBase64` — pin peers by base64 cert fingerprint | **Unchanged** (pure Go) |
| PIN-keyed AES-GCM in `framework/discovery/message` (`SetPin/IsMatchPin/pingToKey/Encrypt/Decrypt`) | **Unchanged** (crypto/cipher) — protocol stays wire-compatible with the phone |
| Go `common/lcv` pipe peer vetting (`ValidateNamedPipeServer/Client/Pid/Assembly`) | Replace with Unix socket permissions + uid/pid checks via `SO_PEERCRED` |
| `QuantumValidator.dll` — Lenovo entitlement check (`/cdn/installer/sc/checkMTCode`, SMBIOS MT code) | **Drop** (Lenovo licensing, not functionality) |
| `Lenovo.CertificateValidation.Native.dll` — WinTrust/catalog checks | **Drop** (code-signing verification only) |
| Lenovo ID / account (PKCE + token-exchange, `account.uds.lenovo.com`, `cloudbroker.uds.lenovo.com`) | Optional cloud tier — reimplement only if cloud features (remote device list, profiles) are wanted; local pairing works without it |

## Not needed on Linux

- `SmartConnectExt` MSIX + COM Explorer context-menu handler (shell verb only).
- `devcon.exe`, Detours, cppwinrt, Windows toast, WMI/`gopsutil`-win parts.
- `Firewall.exe` (Windows Firewall rules) → replace with documented port list.
- `ludp.dll` telemetry, `QuantumValidator.dll` licensing, `Lenovo.CertificateValidation.Native.dll`.

## Wire-compatibility contract (must match the phone to interop)

1. mDNS/DNS-SD advert on **`_readyformdns._tcp`** [wire-confirmed] with TXT keys
   `deviceId, deviceName, type, os, cap, timestamp, fp` (PC: `fp` **hex SHA-256** + `appType`;
   phone: `Cfp` hex SHA-256). The full record-key list in `02` §6 is the union of the
   advertise + device-record schemas.
2. QR invite (when PC-initiated): literal prefix **`motorolardpconnection`** + JSON
   `{version, wsData{ver,cmd,deviceId,deviceName,fp,token,policies,wsPort,...}, ips?}` [decoded].
3. The phone pins the self-signed PC TLS certificate (QR/endpoint `fp` base64;
   mDNS `fp` hex). Observed control connections send no client certificate.
4. **Connection direction (critical):** control channel = **phone dials PC:wsPort** (from QR);
   media channels = **PC dials the phone** on a phone-side media port negotiated over the
   control WS (observed: 3 separate TLS connections to one phone port). [wire-confirmed]
5. TCP reachability probe on **19527** — present in static analysis but **not observed** in the
   live session; treat as optional/legacy pre-check.
6. Message envelope `{deviceId, srcServiceId, serviceId, msgId, data, localIp, remoteIp}` and the
   ServiceId numbering from `01-process-ipc.md` §4 (3 IPCAM, 9 CLIPBOARD, 15 APP, 16 INPUT, 168 FILES,
   202 PC_SCREENSHARE, …).
7. PIN/QR pairing with PIN-keyed AES-GCM for discovery messages (`pingToKey` = b64(8×bswap32(pin)));
   discovery cipher wire `[16B tag][1B type][12B nonce][ct]`, nil AD.
8. WebRTC (Pion) media/datachannels via the `superlink` SDP flow for mirroring/streaming (media
   framing is inside the TLS tunnels — codec/SDP still needs a decrypted or endpoint capture).

## Suggested MVP order

1. Daemon (Go) with mDNS advertise/scan + fingerprint-pinned WS + ServiceId message router.
2. Pairing (PIN + QR) + device record persistence (`GetPairedDeviceInfoList`).
3. Screen share receive path (Pion + FFmpeg decode) → window/GTK.
4. App streaming receive + input injection back over the data channel.
5. Clipboard, files (FUSE+NFS), then webcam (v4l2loopback).
