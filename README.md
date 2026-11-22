# rfproto

An independent Linux peer for Lenovo Ready For / Motorola Smart Connect, written
in Go with a native SDL3 interface. Pair a phone, browse its shared apps, and try
app streaming or the mobile desktop without a Windows PC.

**Experimental, not a complete Smart Connect replacement.** Discovery, durable
pairing/reconnect, app streaming, mobile desktop, input, and phone audio are
implemented. Earlier physical testing on a moto g34 5G covered app video, desktop
video, pointer/keyboard input, audio delivery, and reconnect. That is not a
compatibility guarantee for other phones or long-running sessions. Native
Wayland windowing/input was tested separately; physical-phone streaming was
tested with X11. Phone consent, encoder/display limits, and codec support apply.

Webcam, reverse microphone capture, file access, and PC control are not
implemented. Experimental screen-share protocol probes are not a working
screen-mirroring feature. Some app icons use generated tiles.

## Install on Linux

Build requirements:

- Go supporting this module (`go.mod` currently declares Go 1.22).
- A C compiler for CGO and `pkg-config`.
- Development libraries exposing `sdl3`, `libavcodec`, `libavutil`,
  `libswscale`, `opus`, and `freetype2` through `pkg-config`.

For example, on Arch Linux:

```sh
sudo pacman -S --needed go gcc pkgconf sdl3 ffmpeg opus freetype2
```

On recent Debian/Ubuntu releases, the corresponding packages are typically
`golang-go build-essential pkg-config libsdl3-dev libavcodec-dev libavutil-dev
libswscale-dev libopus-dev libfreetype-dev`; on Fedora, `golang gcc
pkgconf-pkg-config SDL3-devel libavcodec-devel libavutil-devel libswscale-devel
opus-devel freetype-devel`. Availability varies by release/repositories;
SDL2 is not a substitute for SDL3. Older distributions may need newer packages
or a source-built SDL3.

From this checkout:

```sh
./install.sh                         # prompts for prefix, launcher, confirmation
./install.sh --yes                   # unattended install to ~/.local
./install.sh --yes --no-desktop --prefix "/path with spaces/my-prefix"
./install.sh --help
```

The script checks dependencies and builds the sources before replacing the
installed executable. It does not trust a binary already in the checkout, run
sudo, install packages, or copy keys, captures, caches, or pairing state. Relative
prefixes are relative to your calling directory. Add the prefix's `bin`
directory to `PATH` if necessary. Go may download module dependencies.
The executable dynamically links native libraries: they are required even for
headless use. The installer also adds `PREFIX/share/applications/rfproto.desktop`,
`PREFIX/share/icons/hicolor/scalable/apps/rfproto.svg`, and license notices in
`PREFIX/share/licenses/rfproto`. Remove those plus `PREFIX/bin/rfproto` to uninstall;
runtime state is separate.

To build/run directly from source, use the **whole package**, not just
`go run main.go` (the implementation spans several files):

```sh
go build -o rfproto .
go run . serve -help
```

## Quickstart

Run **`rfproto`** or choose **rfproto** in the applications menu for the GUI with
BLE enabled. For explicit startup options:

Use a trusted LAN shared with the phone. rfproto keeps its identity, pairing
credentials, wallpaper, and pairing QR in `$XDG_STATE_HOME/rfproto` (normally
`~/.local/state/rfproto`), independent of the current directory. `-dir` overrides it.

```sh
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/rfproto"
mkdir -p "$state_dir"
chmod 700 "$state_dir"
rfproto serve -dir "$state_dir" -qr "$state_dir/pairing.png" -ble
```

Scan the GUI's pairing QR, terminal QR, or `pairing.png` from the phone's Ready For /
Smart Connect app. `serve` opens the native launcher by default. Choose an app or **Open mobile
desktop**; approve requests on the phone when prompted. Restart with the same
`-dir` to preserve identity and trust.

For an older checkout's `state/`, keep using `-dir /absolute/path/to/state`
until you move that directory while the server is stopped. Credentials are not
silently imported from the working directory or overwritten.

`-ble` needs BlueZ, a powered adapter supporting the peripheral role, and
permission to register advertisements/GATT services on the system bus.
For example, `bluetoothctl power on` powers an available adapter. The installer
does not change Bluetooth permissions. If address detection chooses the wrong
interface, add `-ip YOUR_LAN_IP` (repeatable). Use `-name MYPC` to name the peer.
`rfproto browse 15` discovers other `_readyformdns._tcp` peers;
`rfproto ble scan -secs 20` inspects BLE advertisements.

### Smart Connect over USB (experimental)

Use a data cable and a phone that offers **Smart Connect** in its USB preferences.
This mode uses USB Ethernet, not ADB or USB display output.

1. Connect the unlocked phone. If Linux doesn't get a USB Ethernet link, enable
   **USB tethering** on the phone first and let Linux obtain an IPv4 address by DHCP.
2. Find the USB network interface and the phone's IPv4 address on it:

   ```sh
   ip -br -4 address
   ip -4 route show dev enp0s20u4
   ```

   Replace `enp0s20u4` with your interface. If the USB link installed a DHCP
   gateway, that gateway is the phone. Otherwise check `ip neigh show dev
   enp0s20u4`. Use the phone's USB address, not its Wi-Fi address.
3. Start rfproto with the interface and phone address, then choose **Smart
   Connect** under **Use USB for** on the phone and follow its prompts:

   ```sh
   rfproto serve -usb-interface enp0s20u4 -usb-peer 192.0.2.2
   ```

   `192.0.2.2` is an example; use the actual phone address. No QR scan, BLE
   advertisement, or root access is required by rfproto.

Use a direct cable to your own phone. The pairing invitation is sent to the
configured USB peer; USB address filtering is not proof of identity. The normal
pair-confirm and saved-key checks still apply. The server listens on the USB
IPv4 address, scopes mDNS to that interface, and limits local WebRTC candidates
to it. If a firewall blocks the connection, allow mDNS and the selected
`-port` (TCP 19527 by default) on the USB link. Media also uses WebRTC UDP ports.
rfproto does not change the firewall or USB network settings.

Keep the same state directory. If the USB link gets a different address after
replugging, restart `serve` with the updated interface and phone address.

### CLI, TUI, and headless pairing

Start the GUI with `rfproto`, then use another terminal:

```sh
rfproto ctl -json state
rfproto ctl apps
rfproto ctl desktop
rfproto ctl launch app APP_ID_FROM_APPS
rfproto ctl orientation SESSION_ID portrait
rfproto ctl focus SESSION_ID
rfproto ctl stop SESSION_ID
rfproto ctl mute true
rfproto ctl -json wallpaper
rfproto tui
```

`ctl` and `tui` use the active state directory's private `control.sock`, not a shared
TCP port. For `serve -dir /some/state`, use `ctl -dir /some/state state` or
`tui -dir /some/state`. Flags work before or after the command. Explicit TCP is
still available with `serve -ctl 127.0.0.1:19530` and matching client `-addr`.
Use the full app `id` from `ctl apps`, not its package name alone.

Bare `rfproto ctl` opens the TUI. Stream commands target an active phone display;
focus/mute need the native frontend. `serve -ui=false` is for headless pairing
and state queries, not a headless media player.

### Windows, keyboard, and orientation

The SDL3 workspace has a launcher and a separate window for each stream.
The launcher offers desktop display sizes of 1280×720 and 1920×1080; actual app
frame sizes also depend on the phone. **Landscape / Portrait** request a real
phone virtual-display resize; the UI and `ctl` report it as pending until the
phone replies. Rejections leave working video running and show the error.
**Auto** restores the phone's natural display sizing and follows its rotation
metadata, including both landscape directions and upside-down portrait.
The renderer and input use the same rotation mapping; there is no extra local
“force landscape” turn. These controls do not change Android's global rotation lock.

Use the stream's fullscreen control or **F11** to toggle fullscreen, and **Esc**
to leave it. Keyboard, text, pointer, and supported touch input go to the focused
stream; compositor-reserved shortcuts stay with the compositor. Closing a stream
stops it; closing the workspace releases held input and stops its sessions.

**Send to system tray** hides the launcher and stream windows without stopping
sessions. Click the tray icon to restore them. This needs a StatusNotifier-compatible
tray host (for example KDE; GNOME may need an AppIndicator extension). If no host is
available, the UI stays visible and reports the error. Host loss restores the windows.

In the TUI, `Tab` switches apps/open displays; `↑↓` / `j k` select, `Enter` launches
an app or focuses a display, and `/` searches. On the displays pane, `o` changes the
selected stream's orientation and `x` stops it. `d` opens desktop, `m` mutes,
`r` refreshes, `?` shows help, and `q` quits the client (not the server).

SDL automatically chooses Wayland or X11. To select explicitly:

```sh
SDL_VIDEODRIVER=wayland rfproto serve -dir "$state_dir" -qr "$state_dir/pairing.png" -ble
# Or use SDL_VIDEODRIVER=x11
```

Run inside the desktop session with its normal display environment. There is
no browser UI or browser media engine: Pion handles WebRTC, FFmpeg decodes video,
Opus decodes audio, and SDL renders/plays it. The bundled Inter font is local;
see its [license](web/Inter-LICENSE.txt).

## Privacy, state, and security

- `-dir` contains the persistent TLS certificate/private key, device identity,
  trusted-phone shared keys, wallpaper, and related caches. Keep it private and
  preserve it across restarts. Deleting it loses local trust.
- The phone pins the TLS certificate using the QR fingerprint. Initial pairing
  requires the current invite token and pair-confirm; reconnect requires the
  saved phone credential. This observed path does not use client TLS certificates.
  Unknown credentials, changed keys, and corrupt trust state fail closed.
- One writer owns a state directory. Stop `serve` before changing trust:

  ```sh
  rfproto peers -dir "$state_dir" list
  rfproto peers -dir "$state_dir" forget 'PHONE_REAL_DEVICE_ID'
  ```

  Forgetting is local only. Remove the PC on the phone too before pairing again.
  Older prototypes that did not save pairing keys also require removing the old
  phone-side entry and scanning a fresh QR.
- Pairing QR output and its image contain an invitation credential. Do not post
  them publicly. Raw capture is off by default; `-capture DIR` records original
  frames **including secrets**. Logs, screenshots, captures, wallpapers, and
  phone metadata may be sensitive. Do not submit them without sanitizing.
- Discovery uses LAN mDNS (`_readyformdns._tcp`); the TLS/WebSocket service
  defaults to TCP 19527. BLE advertises the peer and supports authenticated
  endpoint negotiation. Use only trusted networks and configure firewalls
  deliberately; no automatic firewall changes are made.
- The default HTTP control API lives on `control.sock` with mode `0600` inside the
  private state directory. Processes running as your user can control streams.
  Explicit TCP control has no authentication: keep `-ctl` on loopback and never
  expose it to the LAN/Internet. Disable control with `-ctl ''` if unused. Keep the
  diagnostic `-inject` socket disabled or loopback-only. This is not a security-audited product.

## Development and contributing

With native development dependencies installed:

```sh
go test ./...
go test -race ./...
go build -o rfproto .
```

These are commands to run, not a claim that this checkout has passed them.
Tests cover protocol/authentication, control, media, and frontend behavior;
physical-phone, audio-device, compositor, and Bluetooth testing remain important.
For a bug report, include the phone/app version, Linux distribution, display
backend, reproduction steps, and sanitized errors. Distinguish observed behavior
from protocol guesses. Keep changes focused and readable, add regression tests,
and avoid introducing vendor binaries/assets or private runtime artifacts.

### AI usage

This project is mostly human-written, with some help from **GPT 6.1 Sol** and
**Kimi K3.1 Pro**. AI-assisted pull requests are welcome if they pass tests,
actually work, and are understandable and reviewable—not slop. The human
contributor is responsible for understanding, checking, and maintaining the
submitted work; an AI-generated explanation is not evidence that it works.

## Wiki

The public [wiki](wiki/Home.md) is a collection of Markdown pages:

- [Installation](wiki/Installation.md)
- [Usage](wiki/Usage.md)
- [Troubleshooting](wiki/Troubleshooting.md)

Private investigation notes, captures, and vendor reference material are not
public documentation. Do not include them in source releases or bug reports.

## Contributing and license

Read [CONTRIBUTING.md](CONTRIBUTING.md) before sending a pull request.
Original rfproto code and public documentation are [MIT licensed](LICENSE).
Bundled Inter and external dependencies retain their own licenses; see
[third-party notices](THIRD_PARTY_NOTICES.md).
