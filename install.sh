#!/usr/bin/env bash
# Build this checkout; never install its pre-existing binaries or runtime state.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: ./install.sh [--prefix PATH] [--no-desktop] [--yes|-y]

Build rfproto from this checkout and install PREFIX/bin/rfproto.
On a terminal, prompt for the prefix, desktop integration, and confirmation.
Default prefix: $HOME/.local. Relative prefixes use the caller's directory.
  --prefix PATH   Set the default prefix for the prompt (or for --yes).
  --no-desktop    Do not install the applications-menu launcher or icon.
  --yes, -y       Accept the plan without prompts; required without a terminal.
  --help, -h      Show this help.
Requires Linux, Go, a CGO C compiler, pkg-config, SDL3, FFmpeg development
libraries (avcodec, avutil, swscale), Opus, and FreeType. No sudo or package
installation is performed. Go may download module dependencies.

The executable and license notices are always installed; the launcher and icon
are optional (default yes). Nothing is built or installed before confirmation.
No keys, pairing state, caches, or background service are copied.
Runtime state defaults to $XDG_STATE_HOME/rfproto or ~/.local/state/rfproto.
Use rfproto serve -dir PATH to override it; the pairing PNG follows that directory.
EOF
}

prefix=${HOME:?HOME must be set}/.local
assume_yes=0
install_desktop=1
while (($#)); do
    case $1 in
        -h|--help) usage; exit 0 ;;
        -y|--yes) assume_yes=1; shift ;;
        --no-desktop) install_desktop=0; shift ;;
        --prefix)
            if (($# < 2)) || [[ -z $2 ]]; then
                printf '%s\n' 'error: --prefix requires a nonempty path' >&2
                exit 2
            fi
            prefix=$2
            shift 2
            ;;
        --prefix=*) prefix=${1#*=}; shift ;;
        *) printf 'error: unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
    esac
done
cancel() {
    printf '%s\n' 'Cancelled; nothing was built or installed.'
    exit 0
}
if ((!assume_yes)); then
    if [[ ! -t 0 ]]; then
        printf '%s\n' 'error: interactive installation requires a terminal; pass --yes (or -y) for unattended installation' >&2
        exit 2
    fi
    if ! IFS= read -r -p "Installation prefix [$prefix]: " answer; then cancel; fi
    if [[ -n $answer ]]; then prefix=$answer; fi
    if ((install_desktop)); then
        while true; do
            if ! IFS= read -r -p 'Install desktop launcher and icon? [Y/n]: ' answer; then cancel; fi
            case $answer in
                ''|[Yy]|[Yy][Ee][Ss]) break ;;
                [Nn]|[Nn][Oo]) install_desktop=0; break ;;
                *) printf '%s\n' 'Please answer yes or no.' >&2 ;;
            esac
        done
    fi
fi
[[ -n $prefix ]] || { printf '%s\n' 'error: prefix must not be empty' >&2; exit 2; }
[[ $prefix = /* ]] || prefix=$PWD/$prefix
source_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
printf '\nInstallation plan:\n  Build checkout: %s\n  Executable: %s\n  License notices: %s\n' \
    "$source_dir" "$prefix/bin/rfproto" "$prefix/share/licenses/rfproto"
if ((install_desktop)); then
    printf '  Launcher: %s\n  Icon: %s\n' \
        "$prefix/share/applications/rfproto.desktop" "$prefix/share/icons/hicolor/scalable/apps/rfproto.svg"
else
    printf '%s\n' '  Desktop integration: skipped (existing files are left unchanged)'
fi
printf '%s\n' '  Runtime state: unchanged; no credentials or caches are copied.' \
    '  No sudo or system-package installation; Go may download build dependencies.'
if ((!assume_yes)); then
    while true; do
        if ! IFS= read -r -p 'Proceed with build and installation? [y/N]: ' answer; then cancel; fi
        case $answer in
            [Yy]|[Yy][Ee][Ss]) break ;;
            ''|[Nn]|[Nn][Oo]) cancel ;;
            *) printf '%s\n' 'Please answer yes or no.' >&2 ;;
        esac
    done
fi

 guidance() {
    local ID= ID_LIKE=
    if [[ -r /etc/os-release ]]; then . /etc/os-release; fi
    printf '\nInstall development dependencies with your distribution package manager:\n' >&2
    case " $ID $ID_LIKE " in
        *arch*) printf '%s\n' '  pacman -S --needed go gcc pkgconf sdl3 ffmpeg opus freetype2' >&2 ;;
        *debian*|*ubuntu*) printf '%s\n' '  apt install golang-go build-essential pkg-config libsdl3-dev libavcodec-dev libavutil-dev libswscale-dev libopus-dev libfreetype-dev' >&2 ;;
        *fedora*|*rhel*) printf '%s\n' '  dnf install golang gcc pkgconf-pkg-config SDL3-devel libavcodec-devel libavutil-devel libswscale-devel opus-devel freetype-devel' >&2 ;;
        *suse*) printf '%s\n' '  zypper install go gcc pkg-config libSDL3-devel libavcodec-devel libavutil-devel libswscale-devel libopus-devel freetype2-devel' >&2 ;;
        *) printf '%s\n' '  Go, C compiler, pkg-config, SDL3, FFmpeg avcodec/avutil/swscale, Opus, FreeType (development packages).' >&2 ;;
    esac
    printf '%s\n' 'Package names/availability vary by release; SDL3 may require a newer distribution' 'or a source install. FFmpeg development packages may require an additional repository.' 'Run package commands yourself with the appropriate administrator privileges.' >&2
}
[[ $(uname -s) = Linux ]] || { printf '%s\n' 'error: this installer supports Linux only' >&2; exit 1; }
missing=0
for tool in go pkg-config install mktemp mv; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        printf 'error: missing command: %s\n' "$tool" >&2
        missing=1
    fi
done
if ((missing)); then guidance; exit 1; fi
cc=$(cd -- "$source_dir" && go env CC)
# Go accepts compiler arguments in CC; check its executable, then let go build
# perform the authoritative compiler/linker check before touching the target.
read -r -a cc_words <<< "$cc"
if ((${#cc_words[@]} == 0)) || ! command -v "${cc_words[0]}" >/dev/null 2>&1; then
    printf 'error: CGO compiler not found: %s\n' "$cc" >&2
    guidance
    exit 1
fi
for package in sdl3 libavcodec libavutil libswscale opus freetype2; do
    if ! pkg-config --exists "$package"; then
        printf 'error: missing pkg-config dependency: %s\n' "$package" >&2
        missing=1
    fi
done
if ((missing)); then guidance; exit 1; fi

build_dir=$(mktemp -d)
staged=
cleanup() {
    rm -rf -- "$build_dir"
    if [[ -n $staged ]]; then rm -f -- "$staged"; fi
}
trap cleanup EXIT
printf '%s\n' 'Building rfproto from source (CGO enabled)...'
(cd -- "$source_dir" && CGO_ENABLED=1 go build -o "$build_dir/rfproto" .)
# Stage beside the destination, then rename: a failed build/install leaves the
# previous executable intact, and an existing symlink is replaced, not followed.
mkdir -p -- "$prefix/bin"
staged=$(mktemp "$prefix/bin/.rfproto.XXXXXXXX")
install -m 0755 -- "$build_dir/rfproto" "$staged"
if [[ -d $prefix/bin/rfproto ]]; then
    printf 'error: destination is a directory: %s\n' "$prefix/bin/rfproto" >&2
    exit 1
fi
license_dir=$prefix/share/licenses/rfproto
install -d -- "$license_dir"
install -m 0644 -- "$source_dir/LICENSE" "$source_dir/THIRD_PARTY_NOTICES.md" "$license_dir/"
install -m 0644 -- "$source_dir/web/Inter-LICENSE.txt" "$license_dir/Inter-LICENSE.txt"
if ((install_desktop)); then
    # Desktop Exec has both Desktop Entry string escaping and command quoting.
    desktop_exec=$prefix/bin/rfproto
    desktop_exec=${desktop_exec//\\/\\\\}
    desktop_exec=${desktop_exec//\"/\\\"}
    desktop_exec=${desktop_exec//\`/\\\`}
    desktop_exec=${desktop_exec//\$/\\\$}
    desktop_exec=${desktop_exec//%/%%}
    desktop_exec=${desktop_exec//\\/\\\\}
    while IFS= read -r line; do
        if [[ $line = Exec=* ]]; then
            printf 'Exec="%s"\n' "$desktop_exec"
        else
            printf '%s\n' "$line"
        fi
    done < "$source_dir/rfproto.desktop" > "$build_dir/rfproto.desktop"
    if command -v desktop-file-validate >/dev/null 2>&1; then
        desktop-file-validate "$build_dir/rfproto.desktop"
    fi
    install -d -- "$prefix/share/applications" "$prefix/share/icons/hicolor/scalable/apps"
    install -m 0644 -- "$source_dir/web/rfproto.svg" "$prefix/share/icons/hicolor/scalable/apps/rfproto.svg"
    install -m 0644 -- "$build_dir/rfproto.desktop" "$prefix/share/applications/rfproto.desktop"
fi
mv -fT -- "$staged" "$prefix/bin/rfproto"
staged=
printf 'Installed %s\n' "$prefix/bin/rfproto"
if ((install_desktop)); then
    printf 'Launcher: %s\n' "$prefix/share/applications/rfproto.desktop"
    if command -v update-desktop-database >/dev/null 2>&1; then
        update-desktop-database "$prefix/share/applications"
    fi
fi
printf '%s\n' 'License notices are in PREFIX/share/licenses/rfproto. No runtime state was installed.'
