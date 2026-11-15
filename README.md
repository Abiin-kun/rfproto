# rfproto — independent Linux peer for Lenovo Ready For / Smart Connect

A Go implementation of the documented discovery and pairing protocol, with a
native SDL3 workspace for experimental phone app streaming and mobile desktop.
This is not a complete Smart Connect replacement.
The protocol evidence and its limitations are recorded in `docs/`.

## What it does

- `serve` — advertises `_readyformdns._tcp` over mDNS, generates a self-signed cert, starts a
  fingerprint-pinned TLS/WebSocket listener, and emits a **pairing QR** in the exact wire format.
  It logs everything the phone does: TLS ClientHello, client cert (if any), the HTTP/WS upgrade
  headers, and the raw bytes of the first messages — so we can see what the phone expects next.
- `browse` — dumps every `_readyformdns._tcp` advert on the LAN (point this at the real Windows PC
  to read its exact service name + TXT values).
- `ble scan` / `ble adv` — watches LE advertisements (decoding Ready For payloads) and poses as the
  **PC BLE peripheral** (service `0x5246`, characteristics `0x0726`/`0x0722`) via BlueZ.

## Wire facts baked in (docs/05-route-a-crypto.md)

- mDNS service type = `_readyformdns._tcp` (wire-confirmed; override with `-service`).
- QR payload = literal prefix `motorolardpconnection` + JSON
  `{version,wsData{ver,cmd,deviceId,deviceName,fp,token,policies,wsPort,...},ips?}`.
- The phone scans the QR, pins our TLS cert against the QR `fp` (= base64 sha256 of our cert), then
  dials our `wsPort`. Because we compute `fp` from the cert we present, the pin matches by construction.

## Test it

```sh
go build -o rfproto .

# 1. See the real PC's advert (run while the Windows app is discoverable):
./rfproto browse 15

# 2. Pose as a PC and show a pairing QR (use your real LAN IP):
./rfproto serve -ip <your-lan-ip> -name MYPC -ble
#    -> scan the terminal QR (or qr.png) with the phone's Ready For app.

# 3. (optional) watch LE adverts, e.g. what the phone broadcasts:
./rfproto ble scan 20
```

### Native phone workspace

`serve` opens an SDL3 window by default. There is no browser, local web UI, or
browser media engine. The Go peer handles WebRTC with Pion; FFmpeg decodes video,
Opus decodes phone audio, and SDL renders the display and plays audio. Launches,
signaling, input, and teardown still use the authenticated phone connection.

Build prerequisites: Go, a C compiler, `pkg-config`, and development libraries for
SDL3, FFmpeg (`libavcodec`, `libavutil`, `libswscale`), Opus, and FreeType.
For Arch Linux:

```sh
sudo pacman -S --needed go gcc pkgconf sdl3 ffmpeg opus freetype2
go build -o rfproto .
./rfproto serve -ble
./rfproto serve -ble -ui=false  # headless pairing/control server
```

Use the installed-app list to launch an app, or choose **Open mobile desktop**.
The workspace controls display size, session selection/closure, fullscreen, and
audio muting. Keyboard, pointer, and supported touch input target the selected
phone display; closing the workspace releases held input and stops its sessions.
The bundled Inter font is used locally; its license is in `web/Inter-LICENSE.txt`.

The old address-valued `-ui 127.0.0.1:19529` option is removed; `-ui` is now a
boolean. This build requires the native libraries even when using `-ui=false`.
Desktop admission and codec availability remain subject to the phone's consent,
capabilities, and display/encoder limits. The device-specific verification below
does not establish compatibility with every Smart Connect phone.

#### Wayland and X11

The workspace uses SDL windowing, rendering, keyboard/text, pointer, and touch
events without X11 hooks or global input capture. SDL selects the session's
backend automatically. To require native Wayland explicitly:

```sh
SDL_VIDEODRIVER=wayland ./rfproto serve -ble
```

Run this inside the Wayland session with its normal `WAYLAND_DISPLAY` and
`XDG_RUNTIME_DIR`; XWayland is not required. Use `SDL_VIDEODRIVER=x11` to select
X11 explicitly. Input is delivered only while the workspace display has focus.
Compositor-reserved shortcuts remain with the compositor.

#### Native verification

Local checks passed:

- `go test -race . -run 'TestNative|TestPhoneAudio|TestStreaming|TestDesktopRefusal' -count=1`
  and `go build -o rfproto .`.
- Real Pion loopback RTP decoded VP8/H.264 red frames and nonzero Opus PCM.
  Regression cases cover H.264 fragmentation, held-input release, pointer
  mapping, keyboard/text symmetry, and authoritative media teardown.
- A throwaway native smoke program rendered decoded H.264 through an SDL texture
  (readback RGBA `253,0,0,255`) and submitted 960 decoded stereo Opus samples to
  SDL's dummy audio device. This does not verify physical speaker output.
- The actual workspace opened with both `video=wayland` and `video=x11`.
  Native Wayland ran under isolated nested KWin without XWayland; screenshots
  confirmed rendering, entered search text, and pointer-driven size selection.

#### Physical phone verification

Verified on a moto g34 5G over the authenticated LAN connection:

| Mode | Observed behavior |
|---|---|
| App streaming | 215-app catalog; Clock and Files video at 360 × 800 (VP8); pointer navigation and typed text; Opus audio; remote display and audio teardown |
| Mobile desktop | 1280 × 720 desktop; app drawer pointer control and typed search text; audio; remote display teardown |

A generated 440 Hz tone was recovered from the PC output monitor in both modes.
This verifies audio delivery to the desktop audio system, not subjective speaker
quality. Reverse microphone capture remains disabled.

The live run fixed two defects: the phone's feature replies use `senderId:1`
(outbound subsystem routing must not reject inbound replies), and native input
tracking must be forgotten when authoritative state removes a session rather
than sending a second release to the removed session.

Two pre-fix receiver crashes on reopening were traced to a native RGBA overwrite:
FFmpeg's SSSE3 converter writes complete 16-pixel blocks. Output now uses
`av_image_alloc` with 64-byte alignment and passes its padded stride to SDL.
Portrait reopen regressions and three guarded 60-frame decode/render generations
passed; the user also manually confirmed reopening without a crash. Pairing,
fingerprint validation, and memory protections were not disabled.

The physical run used SDL's X11 backend. Native Wayland windowing and input were
verified separately under nested KWin; physical-phone streaming on a Wayland
session has not been separately exercised. Other phones, codecs, touch hardware,
and long-duration stability remain outside this verification.


`-ble` makes `serve` also register a BlueZ LE advertisement + GATT application, which is what the
real PC does continuously (`ble.onBleState` → `startAdvertise`); see `docs/05-route-a-crypto.md` §12.3.
Requires BlueZ with a powered adapter supporting the `peripheral` role (`bluetoothctl power on`).

When the phone scans, it connects and immediately speaks. The server logs and (with `-capture`)
saves everything it sends to `capture/`, e.g. `capture/001_op1_1124B.txt`.

By default the server now **replies** with the recovered PC pairing message (`-reply auto`; see
`docs/05-route-a-crypto.md` §12 and `reply.go`), and then answers the phone's **pair-confirm** with
the recovered PC pair-ack (`-pair auto`, §12.2). Override either to probe the handshake without
recompiling:

```sh
./rfproto serve -ip <ip> -name MYPC                       # default: recovered "auto" reply + pair-ack
./rfproto serve -ip <ip> -name MYPC -reply echo           # echo the phone's message back
./rfproto serve -ip <ip> -name MYPC -reply '{"data":{...}}'  # send a crafted reply
./rfproto serve -ip <ip> -name MYPC -reply ''             # stay silent
./rfproto serve -ip <ip> -name MYPC -replytoken OK        # change data.token in the auto reply
./rfproto serve -ip <ip> -name MYPC -replyeach            # reply to every message
./rfproto serve -ip <ip> -name MYPC -pair off             # no pair-ack
./rfproto serve -ip <ip> -name MYPC -paircmd 2            # simulate "PC refused the pairing"
./rfproto serve -ip <ip> -name MYPC -share mirror         # after the pair-ack, ask for screen mirror
./rfproto serve -ip <ip> -name MYPC -share extend         # ... or second-display ("extend") mode
```

Useful flags: `-port` (default 19527), `-service` (A/B `_readyformdns._tcp` vs `_readyfor._tcp`),
`-ip` (repeatable; the address the phone should dial), `-qr path.png` / `-noqr`, `-wfd` (WiFi-Direct
name), `-token` (fix the QR token), `-capture dir` (save every frame; each WebSocket session gets its
own `dir/c<N>/`, numbered from the highest existing one so restarting `serve` no longer merges two
runs into the same directory), `-reply` / `-replytoken` /
`-replyeach`, `-pair` / `-paircmd`, `-share mirror|extend` (send the recovered `serviceId 202`
screen-share request right after the pair-ack; docs/03 §10), `-sharedelay` / `-shareid` / `-sharerepeat` /
`-shareinterval` (tune that send: delay after the pair-ack, which phone id to name in `data.deviceid` —
`device` or the stable `real`/`realDeviceId` — and how many times to (re)send it), `-set key=value`
(override any mDNS TXT), `-rsa`.

BLE flags: `-ble` (advertise + GATT app), `-blegatt` (default true), `-blecap N` (byte 1 of the
payload; default -1 = low byte of `-cap`, so `0x28ba` → `0xba`), `-blehex` (override the whole
payload, hex), `-bleuuid128` (use the 128-bit UUID form; needs extended advertising), `-blelocalname`
(default true). Standalone: `rfproto ble adv [-id X] [-name N] [-cap N]` and `rfproto ble scan [secs] [-id X]`.

`-cap` is the single source of truth for the PC's capability word (`device.Device+0x58`, default
`0x28ba` = 10426): it drives the mDNS TXT `cap=`, the WS reply's `device.cap` and the BLE advert byte.
`-type`/`-os` default to the real PC's `4`/`6` (same values the real QR and WS reply use).

Identity flags: `-sn` (`device.Device+0x38`; the PC sends its Windows `MachineGuid` — default here is
`/etc/machine-id` re-formatted as a 36-char GUID) and `-sku` (`device.Device+0x48`; the PC sends the
SMBIOS product name — default `/sys/class/dmi/id/product_name`). The phone **rejects a null `sn`**:
its `remote_device` table has `sn NOT NULL` and the app crashes back to the scan screen without it
(docs/05 §12.5). `-skukey Sku|sku|both` picks the JSON key for `sku` (winapi.dll tags it `Sku`, the
phone spells it `sku`); default is the recovered tag.

## Wire facts confirmed by a live pairing (capture/, raw/pairing_session.txt)

- The phone connects over **TLS 1.3**, presents **no client cert** (not mutual TLS at this stage),
  and accepts our cert (QR `fp` pin works).
- WebSocket request path is exactly **`GET /readyfor/`**.
- First frame: a **masked text frame** of ~1124 bytes of **JSON**, envelope `{"data":{...}}`.
  `data.device` carries the phone's `deviceId`, `deviceName`, `cap`, `i3`, `minVerCode{...}`, etc.,
  and `data.token` **echoes our QR token** verbatim.
- If the server stays silent, the phone closes after ~5 s with **`3123: verify failure`**.
- With the recovered reply (§12) the close is gone: the phone sends 8 more frames
  (`capture/002..009`) — the **pair-confirm** (`paired:1` + a 32-byte `key`, which our `-pair auto`
  now echoes back per §12.2) plus service-init messages — then drops the socket with a plain
  `1006`.
- In the 2026-10-17 run the phone then displayed **"turn on bluetooth on both devices"** even though
  its Bluetooth was on: the phone pairs over Wi-Fi, then looks for the PC **over BLE**, and the PC's
  BLE peripheral was the missing piece (docs/05-route-a-crypto.md §12.3).
- With `-ble` the phone got **past** that: it reached the app's **"Get started" page** and sent a
  fuller burst (8 frames, including both DeviceStatus messages on `serviceId 25`), then returned to
  the scan screen. Our reply had five **wrong identity fields** (`caps` was an array, `cap` was
  missing, `verCode`/`verName` were the *phone's*, `serviceVersions` was incomplete) — all now
  recovered exactly from `device.CurrentDevice` (docs/05 §12.4).
- The return to the scan screen is now fully explained: with `adb logcat` attached, the corrected
  reply is **parsed and accepted** (`RemoteClient{cap=10100010111010', type=4, os=6, scType=2,
  verCode=900001000}`), but it lacked `device.sn`, so the app's `remote_device` insert throws
  `NOT NULL constraint failed: remote_device.sn`, `VersionMatchingDialogActivity` then NPEs, the
  process dies, and `AddDevicesActivity` (the scan screen) restarts. A more correct reply fails
  *faster* because the app gets further into its commit path (docs/05 §12.5).
- **With `sn`/`Sku` added the session holds**: `onDeviceOnline` reports
  `StationDevice{sn='500232f2-d839-d2a0-ab5a-ce92693023c8', deviceName='MYPC', …}`, no crash, and the
  phone keeps re-reading the peer address once a second while streaming ~761 KB at us (its wallpaper,
  app list, notifications, status) against the 3 KB we send (hello + pair-ack). See docs/03 §8.

## Durable pairing and reconnect

The normal `serve` path now authenticates the first manager message:

- New pairing requires the **current QR invite token**. No feature messages are
  handled until the phone sends a valid pair-confirm.
- Pair-confirm installs the phone's stable `realDeviceId` and base64 32-byte shared
  key in `state/peers.json` **before** sending the successful acknowledgment.
  `deviceId` may change; the stable identity is the trust-store key.
- A reconnect hello with `paired:1` must supply that phone's saved `data.device.key`.
  A matching QR token does not bypass an incorrect reconnect key.
- Authenticated reconnect replies carry `paired:1`, `device.isPaired:true`, and the
  saved shared key. No client TLS certificate is required on this observed path;
  the phone pins the PC certificate and the PC checks the application credential.
- Malformed hellos, unknown paired phones, and incorrect credentials close with
  `3123: verify failure`. A changed identity/key in pair-confirm is rejected.
  Stored keys are never silently replaced.
- Endpoint refresh uses authenticated binary negotiation on `/readyfor/mdns`
  or BLE `0722`. mDNS TXT `fp` is **hex**; the encrypted endpoint announcement
  and QR use **base64**. Negotiation uses AES-GCM with the 17-byte sender/type
  header as AAD. Plaintext reconnect fallback is deliberately unsupported.

```sh
go build -o rfproto .
./rfproto serve -ip <your-lan-ip> -name MYPC -ble
# Scan the QR once. Restart with the same -dir (default: state).
./rfproto peers list

# Stop serve before removing local trust:
./rfproto peers forget '<phone-realDeviceId>'
# For a nondefault state directory, flags precede the subcommand:
./rfproto peers -dir /path/to/state list
```

**Migration:** earlier prototypes acknowledged pairing without saving the key.
Remove that PC from the phone's paired-device list, then scan a fresh QR once.
After locally forgetting a phone, remove the PC on the phone too before re-pairing.
`peers forget` removes local trust only; it sends no remote unpair command.

The state directory is mode `0700`; newly written trust files are `0600`, with
file/directory sync and atomic replacement. One writer owns a state directory;
another server or a concurrent `forget` fails instead of overwriting trust.
`peers list` is available while serving and omits all shared keys. Corrupt state
fails closed rather than silently resetting the trust list.

Raw capture is now **off by default**. Explicit `-capture <dir>` preserves original
frames, including secrets, in private session directories and mode-`0600` files.
Manager credentials are omitted from normal frame previews. The QR/BLE invite
output still intentionally contains the pairing token: treat that output as a
credential. Historical captures/logs already in this folder are not sanitized and
must not be distributed as implementation assets.

**Verification:** targeted race-enabled tests cover trust persistence, nested
hello credentials, encrypted LAN negotiation, rejection of plaintext/tampered
frames, and real D-Bus delivery of the encrypted BLE reply. Physical reconnect
after a server restart was confirmed on the phone: BLE endpoint negotiation was
accepted as secure, followed by the Wi-Fi control connection. The LAN negotiation
endpoint also has local TLS/WebSocket coverage; that is not a claim that every
phone/network combination is verified. PIN pairing and optional BLE `0728`
fragmentation are not implemented; `0728` is not advertised.

Feature status must use **`senderId:0`**, selecting the phone's mobile-desktop
subsystem. `senderId:1` selects Ready For Assist and does not populate the
mobile-desktop capability/battery cache. This routing correction was verified
on the physical phone: seven tiles appeared and battery status updated.
Capabilities were not inflated. Tile visibility alone does **not** prove feature
support. Experimental app-streaming/mobile-desktop client paths are now present
in the native workspace; webcam, file access, and PC-control are not implemented.

**Implementation boundary:** continuation uses protocol documentation/captures,
not vendor code, UI bundles, libraries, or assets. This does not certify the
provenance of earlier prototype code or constitute a formal two-team clean room.
Diagnostic reply/pair overrides remain probes, not an authentication bypass;
durable initial pairing requires the normal `-pair auto -paircmd 1` behavior.

## State

`state/` holds the persisted cert/key/device-id and trusted-phone credentials.
Keep it private and preserve it across restarts. Deleting it also loses all local trust.
The cert is stable across runs here (easier to debug) — the real app regenerates per session, but
since the QR always carries the *current* cert's `fp`, the pin matches either way.

`capture/c1/` merges two runs (15:01 and 15:28 2026-10-17) because that session numbering bug was
only fixed afterwards; from `capture/c2/` on, one directory is one WebSocket session.

## Historical protocol status / remaining work

**Discovery + pairing + staying connected works.** mDNS advert, QR, fingerprint pin, TLS, WS upgrade,
the phone's hello, the server reply and the pair-ack are all recovered from the binary and observed on
the wire; pairing completes (`paired:1` with a shared key); and with `device.sn`/`device.Sku` in the
reply the phone keeps the session up — no crash, `onDeviceOnline` with a real `sn`, a connection
notification, and traffic flowing for as long as the server is up
(`raw/logcat_2026-10-17_success.txt`, `capture/c1/`, docs/03 §8).

The **BLE peripheral** (the PC's other half) is implemented too: `serve -ble` registers the
`0x5246` advertisement (`[0x11][cap][deviceId]` service data) and the GATT application over BlueZ.
The primary `0x0722` characteristic supports **write-with-response + indications**;
`0x0726` supports write-without-response + notifications. A paired request is an
authenticated binary frame, and its encrypted endpoint answer is sent through
BlueZ's emitted `Value` property change. The phone accepted this exchange as
`channel:1, isSecure:true` during the successful reconnect.

Earlier notes misidentified connectivity `channel:3` as BLE. It is **Wi-Fi/mDNS**;
channel `1` is BLE. The old readable-JSON GATT probe and its assumption-based tests
have been removed. See `docs/05-route-a-crypto.md` §13 for the corrected contract.
The phone's current `deviceId` can rotate while `realDeviceId` remains stable.

The PC's `device.Device` identity is now **exact** rather than guessed (docs/05 §12.4/§12.5):
`cap=0x28ba`, `caps`/`serviceVersions`/`minVerCode` maps, `verCode=900001000`,
`verName="09.0.0.001.000"`, `sn` (the Windows `MachineGuid`) and `Sku` (the SMBIOS product name).
Previously `caps` was serialised as a JSON **array** where the phone expects a map, and `sn` was
absent — the latter made the app crash back to the scan screen.

What the phone does next is captured but not yet answered (docs/03 §8): it announces a **file-station
channel** (`serviceId 168`, `port`/`fp`/`fp2`, polled once a second) and an **app-icon channel**
(`serviceId 17`, `port` + `token` + `fingerprint`, `appLimit:5`, 207 apps), and pushes its wallpaper
(`serviceId 25 cmd 2`, base64 PNG). Those announces are the entry points for scope item 2 (app
streaming / file access) — but the announced port is **not** directly reachable, and not even
listening: `192.168.1.100:55218` and `:54337` both answer `connection refused` and the phone's own
socket table lists no listener on either, while the next session announces a *different*
port/token/fingerprint. It is opened on demand, and the real PC reaches it through an in-session
**tunnel** (`framework/connectivity.ForwardServicePort` → `tunnel.Forward`, multiplexed over the WS as
`p2p` frames, then fetched from `127.0.0.1:<forwardPort>` with the announce's token, pinning its
fingerprint). That is the next sub-project, specced in docs/03 §9.

The **control half of screen share is implemented**: `serve -share mirror|extend` sends the recovered
`serviceId 202` request right after the pair-ack, addressed to the phone id learned from its own
frames. The frame matches the real PC byte shape — `data` is a JSON *string* holding the share JSON,
wrapped with `cmd:25` and `ver:"1"`, and the envelope carries **our** id (docs/03 §10.1). The corrected
frame was then sent alone and the phone **ignored it** — no 202 reply, no
screen-share/`MediaProjection`/RDP log line, no consent dialog (docs/03 §10.3). So 202 is not a
standalone trigger; the real PC pairs it with the native, driver-backed path. The earlier idea that the
call site also emits a preceding `services/WebView.Connect` frame (services 33/1/PC_UI) is **refuted**:
`WebView.Connect`'s `cl == 0xca` branch only updates PC-side WebView state via a *local* broadcast and
writes nothing to the socket, so the 202 `cmd:25` message is the PC's only screen-mirror frame
(docs/03 §10.3). The first attempt used an earlier, structurally wrong version of the frame (object
`data`, no `cmd`/`ver`) and settled nothing (docs/03 §10.2). Two cheap fidelity knobs remain — send
timing and the `data.deviceid` target — see `-sharedelay` / `-shareid` / `-sharerepeat` and docs/03 §10.4.
What follows this message on the real PC — codec negotiation and the video path — is handed to a
separate native process (`ScreenSharePlugin.exe`); docs/03 §10.

Run `./rfproto serve -ip <ip> -ble`, scan, and compare the captured frames against `capture/c1/`
(each new WS session captures into its own `capture/c<N>/`).

## Debugging a run with the phone's own logcat

The phone's log is the fastest oracle for what it does with our reply — it is what pinpointed the
missing `sn` (docs/05 §12.5). With USB debugging on:

```sh
adb devices -l
adb logcat -c -b all && adb logcat -b all > /tmp/logcat.txt &   # arm before you scan
./rfproto serve -ip <ip> -name MYPC -ble
# ... scan the QR, watch the phone ...
kill %1
grep -nE 'MotoDesktopLog|AndroidRuntime|am_crash|am_proc_died|AddDevicesActivity' /tmp/logcat.txt
```

`MotoDesktopLog` carries the app's own `[R4ConnectivityManager]`/`[DBManager]`/`[DeviceConnectManager]`
lines; `AndroidRuntime` carries any `FATAL EXCEPTION` (a crash is why the app lands back on
`AddDevicesActivity`, the scan/pair screen).


