# Contributing to rfproto

Focused, readable, tested, human-reviewed changes are welcome. Describe the
problem and the behavior you changed; keep unrelated cleanup out of the pull
request. Prefer existing patterns and straightforward code over extra layers.
Add regression coverage for fixes and update public user-facing documentation
when behavior or setup changes.

## Set up a Linux development environment

Use Go 1.22 or newer, a C compiler with CGO enabled, `pkg-config`, and native
development packages exposing these package names:

```text
sdl3 libavcodec libavutil libswscale opus freetype2
```

On Arch Linux, for example:

```sh
sudo pacman -S --needed go gcc pkgconf sdl3 ffmpeg opus freetype2
```

Package names and availability vary on other distributions. See the
[README installation instructions](README.md#install-on-linux) and
`./install.sh --help`. SDL2 cannot replace SDL3; older distributions may need newer SDL3
packages or a source installation. The same native libraries are required
for tests and headless use, not just the graphical launcher.

Build the entire Go package, not `go run main.go`:

```sh
CGO_ENABLED=1 go build -o rfproto .
```

To build and install from your checkout:

```sh
./install.sh                         # interactive installation
./install.sh --yes --prefix /your/prefix  # unattended /your/prefix/bin/rfproto
```

The installer does not install system packages or copy runtime state. Keep
pairing state outside the checkout, and use the [README quickstart](README.md#quickstart)
for running against a phone.

## Before opening a pull request

Format the Go files you changed with `gofmt -w` and their explicit paths, then
run the following from the repository root with native dependencies installed:

```sh
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go build -o rfproto .
```

State which checks you actually ran and their results. Report failures,
skipped checks, missing hardware, and environmental restrictions honestly;
do not describe unrun commands as passing. Tests are necessary, but passing
tests alone does not prove phone or native-device behavior.

For changes affecting runtime behavior, provide relevant, sanitized evidence:

- For the native UI: describe the action and observed result, desktop/display
  backend, scaling and window configuration; include screenshots only when
  they are safe to share. Exercise the affected input, focus, resize, or
  orientation paths rather than only showing a static window.
- For media/audio: exercise the affected real stream and playback path; report
  the audio device/backend, whether sound was actually audible, and any
  observed stalls or reconnect behavior. A decoded packet or a successful
  command is not proof that playback worked.
- For phone-dependent behavior: give the phone model, Android version, Ready
  For / Smart Connect app version, and reproduction steps. Separate observed
  results from protocol hypotheses and say when physical-phone testing was
  unavailable. One phone's success is not a compatibility guarantee.

Match runtime proof to the scope of the change. A documentation-only change
does not need a phone session. The project is experimental; do not claim
unimplemented functionality works. Webcam support is not a prerequisite for
contributing or for unrelated pull requests.

Every submission needs human review. Be ready to explain the implementation,
its tradeoffs, and how the evidence supports the claimed behavior.

## Keep submissions public-safe

- Do not commit vendor applications, extracted code, UI bundles, proprietary
  assets, captures, or binaries. Use original or appropriately licensed
  material and preserve required third-party notices.
- Do not publish private investigation notes or local working documents.
  Public documentation should be a deliberately written, sanitized account,
  not a copy of a private notebook or vendor material.
- Never submit runtime state, certificates/private keys, pairing credentials,
  invitation QR codes, phone identities, or secrets. Logs, screenshots,
  wallpapers, and device metadata may also disclose private information.
  Inspect and sanitize attachments before sharing them; prefer a minimal
  reproduction with synthetic, non-sensitive data.
- Removing private files from a working tree does not remove earlier commits.
  Before publishing an existing repository, review its history or create a clean
  public snapshot; never push private research history by accident.
- For a security-sensitive report, do not post live credentials or exploit
  details that expose someone's device in a public issue. Share only the
  minimum sanitized information needed to arrange appropriate handling.

## AI-assisted contributions

This project is mostly human-written, with some help from **GPT 6.1 Sol** and
**Kimi K3.1 Pro**. AI-assisted pull requests are accepted when they pass tests,
actually work, and are understandable and reviewable—not slop.

The human contributor owns the submission: read and understand every change,
check correctness and licensing, run the relevant tests, and obtain real
runtime evidence where applicable. Generated code, comments, and explanations
are not proof that something works. Do not submit unreviewed generated dumps,
fabricated results, or unnecessary abstractions, and do not send secrets or
private/vendor material to an AI service.

## Licensing

By submitting original code or public documentation, you agree to contribute
it under the project's [MIT license](LICENSE). Only submit work you have the
right to license. Third-party material keeps its own license: see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), including the separate Inter
font notice and the requirements for distributing linked binaries. Adding a
dependency requires reviewing its license and updating the relevant notices.
