# Installation

[Home](Home.md) · [Usage](Usage.md) · [Troubleshooting](Troubleshooting.md)

## Linux dependencies

Building requires Go compatible with `go.mod` (currently Go 1.22), a C compiler
for CGO, `pkg-config`, and development libraries for:

- **SDL3** (`sdl3`; SDL2 is not a substitute).
- **FFmpeg** (`libavcodec`, `libavutil`, `libswscale`).
- **Opus** (`opus`).
- **FreeType** (`freetype2`).

For Arch Linux:

```sh
sudo pacman -S --needed go gcc pkgconf sdl3 ffmpeg opus freetype2
```

On recent Debian/Ubuntu releases, package names are typically `golang-go`,
`build-essential`, `pkg-config`, `libsdl3-dev`, `libavcodec-dev`, `libavutil-dev`,
`libswscale-dev`, `libopus-dev`, and `libfreetype-dev`. Fedora commonly uses
`golang`, `gcc`, `pkgconf-pkg-config`, `SDL3-devel`, `libavcodec-devel`,
`libavutil-devel`, `libswscale-devel`, `opus-devel`, and `freetype-devel`.
Availability depends on the distribution release and enabled repositories;
older releases may need newer packages or a source-built SDL3.

## Install from the checkout

Run from the project root in a terminal:

```sh
./install.sh
# Or supply a different default for the prefix prompt:
./install.sh --prefix "/path with spaces/my-prefix"
./install.sh --help
```

The interactive installer prompts for an installation prefix (default
`~/.local`) and whether to install the desktop launcher and icon (default yes).
Press Enter to keep a displayed default. It then shows the resolved installation
plan and asks for confirmation; this final prompt defaults to **no**. Answering
no, or closing input at a prompt, cancels successfully before dependency checks,
compilation, downloads, or installation writes. Nothing in the prefix is changed.
An explicit `--no-desktop` skips the desktop question and disables desktop
installation.

For scripts, CI, or any input that is not a terminal, explicitly accept the plan
with `--yes` (or `-y`):

```sh
# Default prefix, with launcher and icon; no prompts:
./install.sh --yes
# Custom prefix without desktop integration; no prompts:
./install.sh -y --prefix "/path with spaces/my-prefix" --no-desktop
```

Without `--yes`, non-terminal input is rejected with an error rather than
waiting for input or accepting defaults. `--yes` skips all questions but still
prints the plan, checks dependencies, and reports missing tools or libraries.
It uses `~/.local` unless `--prefix` is supplied, and installs desktop integration
unless `--no-desktop` is supplied.

The executable is installed to `PREFIX/bin/rfproto` (by default,
`~/.local/bin/rfproto`); a relative prefix is resolved against your calling
directory. Add that `bin` directory to `PATH` if needed. When selected, the
applications-menu entry is `PREFIX/share/applications/rfproto.desktop`, with
its icon under `PREFIX/share/icons/hicolor/scalable/apps/rfproto.svg`. With the
default prefix it appears as **rfproto** in the desktop menu. A custom prefix's
`share` directory must be in your desktop's XDG data search path to appear there.
`--no-desktop` installs neither file and does not update the desktop database;
it leaves any launcher or icon from an earlier installation unchanged.

After confirmation, the installer checks dependencies and builds the checkout
with CGO enabled before replacing the installed executable. It does not use a
prebuilt checkout binary, run `sudo`, install system packages, change system
configuration, import credentials, or copy runtime state. Installation is scoped
to the chosen prefix, which must be writable by the invoking user. Go may
download module dependencies and update its build cache after confirmation.
Native shared libraries remain required at runtime, including in headless mode.
Project and font license notices are always installed in
`PREFIX/share/licenses/rfproto`, including with `--no-desktop`.

Alternatively, build the whole package from the project root:

```sh
go build -o rfproto .
./rfproto serve -help
```

Do not use `go run main.go`; the implementation spans multiple files. Use
`go run . serve -help` when running directly through Go.

## Keep runtime state private

Choose a persistent private directory before [starting the server](Usage.md#pair-and-start):

```sh
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/rfproto"
mkdir -p "$state_dir"
chmod 700 "$state_dir"
```

rfproto defaults to `$XDG_STATE_HOME/rfproto`, or `~/.local/state/rfproto` if the
environment variable is unset or relative. Use `-dir` to override it. The pairing
PNG defaults to `pairing.png` in that directory; `-qr ''` disables only the PNG.
All normal state paths are independent of the current working directory.

State contains the TLS private key and certificate, local device identity,
trusted-phone credentials, wallpaper, and caches. Preserve it for reconnects;
deleting it loses local identity and trust. Do not commit, publish, or share it.
Pairing QR images and terminal QR output contain an invitation credential.
Logs, screenshots, and phone metadata can also reveal private information.
Raw capture is off by default; `-capture` output can include secrets.

Only one server may own a state directory at a time. Stop it before using the
[peer-management commands](Troubleshooting.md#pairing-or-reconnect-fails).

## Uninstall

Remove `PREFIX/bin/rfproto`, `PREFIX/share/applications/rfproto.desktop`,
`PREFIX/share/icons/hicolor/scalable/apps/rfproto.svg`, and
`PREFIX/share/licenses/rfproto`. Runtime state is separate and is not removed
automatically. Keep it if you want to retain pairing trust.
