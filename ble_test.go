package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestBleAdvPayload pins the byte layout recovered from ble.startAdvertise
// (winapi.dll 0x1806b7b80):
//
//	[0x11][cap&0xff][base64decode(deviceId)]
//
// The deviceId is the same value the prototype advertises in mDNS TXT
// `deviceId=` and in the QR wsData.
func TestBleAdvPayload(t *testing.T) {
	const id = "06T3cwaNbuA4xDLwylLvEw==" // 16 bytes, from state/device_id

	got, err := bleAdvPayload(id, 0)
	if err != nil {
		t.Fatalf("bleAdvPayload: %v", err)
	}
	want, _ := hex.DecodeString("1100d3a4f773068d6ee038c432f0ca52ef13")
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %s, want %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}

	// cap occupies byte 1 (device.Device+0x58, json "cap").
	got, err = bleAdvPayload(id, 0xF5)
	if err != nil {
		t.Fatalf("bleAdvPayload: %v", err)
	}
	if got[0] != 0x11 || got[1] != 0xF5 {
		t.Fatalf("header = %#x %#x, want 0x11 0xf5", got[0], got[1])
	}
	if len(got) != 18 {
		t.Fatalf("len = %d, want 18", len(got))
	}
}

func TestBleAdvPayloadRejectsNonBase64(t *testing.T) {
	if _, err := bleAdvPayload("not base64!!", 0); err == nil {
		t.Fatal("expected an error for a non-base64 deviceId")
	}
}

// TestBleAdvFitsLegacyAdvertising guards the 31-byte legacy limit
// (BlueZ reports SupportedCapabilities.MaxAdvLen=31):
//
//	flags 3 + 16-bit service UUID list 4 + service data (1+1+2+18) 22 = 29
func TestBleAdvFitsLegacyAdvertising(t *testing.T) {
	payload, err := bleAdvPayload("06T3cwaNbuA4xDLwylLvEw==", 0)
	if err != nil {
		t.Fatal(err)
	}
	const (
		flags        = 3
		uuid16List   = 4
		svcDataFixed = 4 // ad length, ad type, 16-bit uuid
	)
	if got := flags + uuid16List + svcDataFixed + len(payload); got > 31 {
		t.Fatalf("advertisement would be %d bytes, over the 31-byte legacy limit", got)
	}
}
