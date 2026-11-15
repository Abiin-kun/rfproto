# rfproto user guide

rfproto is an independent Linux peer for Lenovo Ready For / Motorola Smart
Connect. Its native SDL3 workspace lets you pair a phone, browse shared apps,
and open app streams or a mobile desktop in separate windows.

This `wiki/` directory is the public documentation collection in the project;
it does not require a separately hosted wiki.

## Start here

1. [Installation](Installation.md): dependencies, local installation, and private state.
2. [Usage](Usage.md): pairing, native windows, terminal controls, and headless operation.
3. [Troubleshooting](Troubleshooting.md): connection, display, audio, and pairing problems.

## What to expect

This is experimental software, not an official or complete Smart Connect
replacement. Phone consent, device capabilities, encoder limits, and codec
support affect which streams work. Multiple local windows do not guarantee that
a phone can run every requested stream simultaneously.

App streaming, mobile desktop, input, phone audio, and persistent pairing are
implemented. Webcam, reverse microphone capture, file access, and PC control are
not implemented. Experimental screen-share probes are not a usable
screen-mirroring feature. Some app icons are generated tiles.

Use a trusted LAN. Keep pairing QR codes, keys, phone metadata, and runtime state
private; see [Installation](Installation.md#keep-runtime-state-private).
