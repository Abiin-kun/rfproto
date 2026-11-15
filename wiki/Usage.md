# Usage

[Home](Home.md) · [Installation](Installation.md) · [Troubleshooting](Troubleshooting.md)

## Pair and start

Use a trusted LAN shared with the phone. After [installing](Installation.md), run
**`rfproto`** (no arguments) or open **rfproto** in the applications menu.
This starts the GUI with BLE enabled and private per-user state. For explicit options:

```sh
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/rfproto"
mkdir -p "$state_dir"
chmod 700 "$state_dir"
rfproto serve -dir "$state_dir" -qr "$state_dir/pairing.png" -ble
```

Scan the launcher's pairing QR, terminal QR, or `pairing.png` from the phone's
Ready For / Smart Connect app. Approve requests on the phone. Restart with the same
state directory to keep pairing trust; do not share the QR or credentials.

`-ble` uses BlueZ and needs a powered Bluetooth adapter with peripheral-role
support and permission to register advertisements/GATT services. If necessary,
`bluetoothctl power on` powers an available adapter. Installation does not grant
Bluetooth permissions. Use `-name MYPC` to set the advertised name, or repeat
`-ip YOUR_LAN_IP` for the LAN addresses to advertise when automatic selection
picks the wrong interface.

## Native multi-window workspace

`serve` opens the SDL3 launcher by default. Select a shared app or **Open mobile
desktop**. Each stream gets its own window; the launcher remains available to
open or focus others. Desktop size choices are 1280×720 and 1920×1080. Actual
app dimensions and simultaneous-stream availability depend on the phone.

Each stream has **Auto / Landscape / Portrait** controls:

- **Auto** restores natural phone display sizing and follows the phone's rotation metadata.
- **Landscape / Portrait** request the corresponding virtual-display shape from the phone.

Requests remain pending until the phone replies. A refusal is shown without stopping the
current stream. The picture is rotated from the phone's metadata only, with matching input
mapping; selecting a mode never adds a second local turn. Android's global rotation lock
is not changed. A tiling window manager may retain its tile size while the video fits inside it.

Click **Fullscreen** or press **F11** to toggle fullscreen. The header and
fullscreen button remain visible in fullscreen; the button becomes **Exit full**
so pointer and touch users can leave it. **Esc** also exits fullscreen.

**Send to system tray** hides the launcher and stream windows while sessions and
audio continue. Click the tray icon to restore them. A StatusNotifier-compatible
host is required; unsupported desktops keep the UI visible with an error. If the
tray watcher disappears, rfproto restores the windows automatically.

Keyboard, text, pointer, and supported touch input target the focused stream.
Desktop-compositor shortcuts may be intercepted by the compositor. Closing a
stream stops that session; closing the workspace releases held input and stops
its sessions. The launcher audio control mutes local phone-audio playback.

SDL selects Wayland or X11 automatically. To choose explicitly, set the driver
when starting the server, for example:

```sh
SDL_VIDEODRIVER=wayland rfproto serve -dir "$state_dir" -qr "$state_dir/pairing.png" -ble
# For X11, use SDL_VIDEODRIVER=x11 instead.
```

Run inside your normal desktop session. There is no browser frontend.

## Terminal interface

With the server running, open another terminal:

```sh
rfproto tui
# Bare `rfproto ctl` also opens the TUI in an interactive terminal.
```

| Key | Action |
| --- | --- |
| `Tab` | Switch focus between apps and open displays/sessions |
| `↑` / `↓`, `k` / `j` | Select an item in the focused pane |
| `Enter` | Launch the selected app or focus the selected session window |
| `/` | Search apps; `Esc` clears/exits search |
| `o` | Request the selected session's next orientation in the sessions pane |
| `x` | Stop the selected session in the sessions pane |
| `d` | Open mobile desktop |
| `m` | Toggle local audio mute |
| `r` | Refresh state |
| `?` | Show help |
| `q` | Quit the TUI, not the server |

While typing in search, printable command keys enter search text instead.
Window focus and mute require the native frontend. Orientation requires an active phone display;
the TUI uses the phone-confirmed mode and shows pending requests/errors rather than guessing success.

## CLI and local control API

Flags work before or after subcommands. Use session IDs from `state` and the full app
`id` from `apps` (not its package name alone):

```sh
rfproto ctl -json state
rfproto ctl apps
rfproto ctl desktop
rfproto ctl launch app APP_ID_FROM_APPS
rfproto ctl orientation SESSION_ID portrait
rfproto ctl focus SESSION_ID
rfproto ctl mute true
rfproto ctl stop SESSION_ID
```

The default endpoint is a private UNIX socket at `<state-dir>/control.sock`.
If the server uses `-dir /some/state`, use `rfproto ctl -dir /some/state state`
or `rfproto tui -dir /some/state`. The default state directory is the same as the GUI's.
For an explicit TCP server `-ctl 127.0.0.1:PORT`, pass the matching `-addr` to clients.
An explicit UNIX address uses `-addr unix:/absolute/path/control.sock`.

`GET /state` returns workspace state, including apps, sessions, connection/audio
information, and a top-level JSON boolean `muted` for local frontend audio mute.
Without a native frontend, `muted` is `false`; this is not a phone-side mute
setting. The API also provides `GET /apps`, `GET /wallpaper`, and `POST` routes
`/launch`, `/stop`, `/orientation`, `/focus`, and `/mute`. Prefer `ctl` unless
you need to integrate another client.

Session `width`/`height` are the natural encoder/input raster. `rotation` describes
its presentation; `orientation` is the phone-confirmed selection. A new request appears
as `orientationPending`; rejection/timeout appears as `orientationError`.
`POST /orientation` returns HTTP `202` for request acceptance, not proof of applied pixels.

The UNIX socket is mode `0600` inside the private state directory. Any process running
as your user can control it. Explicit TCP control is unauthenticated: use loopback only,
never expose it to the LAN/Internet, and disable control with `-ctl ''` if unused.
Leave the diagnostic `-inject` socket disabled or loopback-only as well.

## Headless server

Add `-ui=false` to the startup command:

```sh
rfproto serve -dir "$state_dir" -qr "$state_dir/pairing.png" -ble -ui=false
```

Headless mode supports pairing and state queries. It has no local media receiver/player;
open the native GUI before launching streams. Focus and mute need the native frontend,
and orientation requests need an active phone display. Native shared libraries are still required.
