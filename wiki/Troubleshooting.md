# Troubleshooting

[Home](Home.md) · [Installation](Installation.md) · [Usage](Usage.md)

## Build or startup cannot find a library

Install the development packages in [Installation](Installation.md), including
SDL3 rather than SDL2. Ensure `pkg-config` can find `sdl3`, `libavcodec`,
`libavutil`, `libswscale`, `opus`, and `freetype2`. Libraries built outside system
paths may require your distribution's normal library and `pkg-config` path
configuration. Runtime shared libraries are needed even with `-ui=false`.

Build the full package (`go build -o rfproto .`), not only `main.go`.
If `rfproto` is not found after installation, add your prefix's `bin` directory
to `PATH` or invoke the executable by its full path.

## The phone cannot discover or reach the PC

- Put both devices on the same trusted LAN. Guest-network isolation, VPN routes,
  and firewalls can prevent discovery or connection.
- Check the advertised address; use `-ip YOUR_LAN_IP` if automatic selection
  chooses an unreachable interface.
- Discovery uses mDNS (`_readyformdns._tcp`); the TLS/WebSocket listener defaults
  to TCP 19527. Configure your firewall deliberately; rfproto does not change it.
- For `-ble`, check BlueZ, adapter power, peripheral-role support, and system-bus
  permissions. `bluetoothctl power on` does not grant missing permissions.
- Scan the current pairing QR in the phone's app. Keep that QR private.

Optional discovery checks are `rfproto browse 15` and
`rfproto ble scan -secs 20`. These inspect discovery, not successful pairing.
Do not open the local control port to solve phone connectivity problems.

## Pairing or reconnect fails

Use the same persistent state directory on every run. The default is
`$XDG_STATE_HOME/rfproto` or `~/.local/state/rfproto`; `-dir` overrides it.
For an older checkout's `state/`, use `-dir /absolute/path/to/state` until you
move that directory while the server is stopped. rfproto does not silently import
credentials from the working directory or merge/overwrite existing stores.
Do not run two servers against the same directory or manually alter credentials.

If you need to reset trust, stop `serve`, then use the directory you actually run:

```sh
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/rfproto"
rfproto peers -dir "$state_dir" list
rfproto peers -dir "$state_dir" forget 'PHONE_REAL_DEVICE_ID'
```

Replace the placeholder with the phone ID reported by `list`. Forgetting only
changes the PC's trust store. Remove the PC entry on the phone too, restart the
server, and pair using its fresh QR. This also applies to older prototypes that
did not persist pairing credentials. Deleting all state is not required for
routine reconnect troubleshooting and loses the local identity.

## No window, incorrect orientation, or fullscreen confusion

Run without `-ui=false` inside a desktop session with its normal display
environment. If needed, explicitly choose `SDL_VIDEODRIVER=wayland` or
`SDL_VIDEODRIVER=x11`; the selected backend must be available in your session.

**Auto** restores native sizing and follows phone rotation metadata. **Landscape / Portrait**
request a virtual-display resize on the phone; they are no longer local picture rotations.
The pending/error state is visible in the stream and `ctl state`. A refusal or timeout does
not mean the requested orientation was applied. In fullscreen, use **Exit full**, **F11**, or
**Esc**. Focus the intended stream; compositor-reserved shortcuts may never reach it.

**Send to system tray** requires a running StatusNotifier host. KDE supports this;
GNOME may need an AppIndicator extension. Without a supported tray the window
stays visible and shows an error. Clicking the tray icon restores the windows;
losing the tray watcher also restores them rather than leaving the app hidden.

A stream waiting for the phone may need phone-side consent. A failed or blank
stream can also reflect device codec, encoder, or display limits. Try one stream
at a time and report the exact behavior rather than assuming all phones support
the same combinations.

## No audio or terminal controls fail

Check the native launcher's audio mute control and the desktop's selected audio
output. `rfproto ctl -json state` includes the local `muted` boolean. Headless
mode does not play media; focus and mute need the native frontend, and orientation
needs an active phone display.

If `ctl` cannot connect, run the GUI and ensure control is not disabled with `-ctl ''`.
By default clients use the private state-directory `control.sock`, not TCP port 19530.
For a server with a custom `-dir`, pass the same `-dir` to `ctl`/`tui`; for an explicit
`-ctl` address, pass matching `-addr`. Startup reports binding failures instead of silently
leaving clients connected to an older process. Flags may precede or follow commands.
Use the full catalog `id` from `ctl apps` when launching an app.
In the TUI, `Tab` selects sessions before `o`/`x`; `Enter` focuses the selected display.

## Report a problem safely

Include the phone model, phone app version, Linux distribution, Wayland/X11
backend, exact command with secrets removed, reproduction steps, and sanitized
error text. State which behavior you actually observed. Do not attach private
state, pairing QR codes, raw captures, unreviewed logs, wallpapers, or other
phone data. This is experimental software, not a security-audited product.
