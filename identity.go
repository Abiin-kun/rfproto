// Host identity values the PC puts in device.Device and the phone requires.
//
// The phone stores every peer it talks to in a SQLCipher database table
// `remote_device`, whose `sn` column is NOT NULL. A live run against this
// prototype (with adb logcat attached) showed exactly what happens when it is
// missing:
//
//	E MotoDesktopLog: [ DBManager ] - insert RemoteDevice error!
//	E MotoDesktopLog: android.database.sqlite.SQLiteConstraintException:
//	    NOT NULL constraint failed: remote_device.sn (code 1299)
//	I MotoDesktopLog: [ DeviceConnectManager ] - onDeviceOnline
//	    StationDevice{deviceId='06T3...', sn='null', ...} findDevice = false.
//	E AndroidRuntime: FATAL EXCEPTION: ... NullPointerException ...
//	    at ...VersionMatchingDialogActivity.o(SourceFile:119)
//	I ActivityManager: Process com.motorola.mobiledesktop has died
//	I ActivityManager: Start proc ... {com.motorola.readyfor.pairing.ui.AddDevicesActivity}
//
// i.e. the app crashes and returns to the scan/pair screen. Both values below
// are therefore part of the recovered device identity, not decoration.
//
// sn  (device.Device+0x38, json "sn,omitempty")
//
//	device.GetDeviceSn (0x18030e280) reads the registry value
//	HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid and caches it in the
//	globals 0x1813d00f0/0x1813d00f8, logging "deviceSn:%s". That is Windows'
//	stable per-installation identifier, a 36-character hyphenated GUID.
//	Linux keeps the same concept in /etc/machine-id (32 hex characters), so
//	we simply re-format it in the Windows shape.
//
// Sku (device.Device+0x48, json "Sku,omitempty")
//
//	device.getDeviceHardwareInfo (0x18030e9c0) opens
//	HKLM\HARDWARE\DESCRIPTION\System\BIOS, reads SystemManufacturer,
//	SystemFamily and SystemProductName, and caches SystemProductName in the
//	globals 0x1813d0120/0x1813d0128 (logged
//	"getDeviceHardwareInfo, deviceManufacturer:%s, deviceModel:%s, deviceSku:%s").
//	Those two globals are copied into device.Device+0x48 by
//	device.CurrentDevice (0x18030e0ce..0x18030e170). Linux exposes the very
//	same SMBIOS string at /sys/class/dmi/id/product_name.
package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// machineGUID returns a stable per-machine identifier formatted the way the
// Windows PC formats its MachineGuid (8-4-4-4-12 lowercase hex).
func machineGUID(dir string) string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if g := guidFromHex(strings.TrimSpace(string(b))); g != "" {
			return g
		}
	}
	// No machine-id: persist one, so the phone sees a stable identity.
	p := filepath.Join(dir, "machine_guid")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		log.Printf("[id] rand: %v", err)
	}
	g := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
	if err := os.WriteFile(p, []byte(g), 0o644); err != nil {
		log.Printf("[id] write %s: %v", p, err)
	}
	return g
}

// guidFromHex re-formats 32 hex characters (with or without dashes) as a
// hyphenated GUID. Returns "" when the input is not 32 hex characters.
func guidFromHex(s string) string {
	s = strings.ToLower(strings.NewReplacer("-", "", " ", "", "\n", "", "\r", "").Replace(s))
	if len(s) != 32 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[0:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

// machineProductName returns the SMBIOS product name, the value the Windows PC
// reports in device.Device+0x48 (json "Sku").
func machineProductName() string {
	for _, p := range []string{
		"/sys/class/dmi/id/product_name",
		"/sys/class/dmi/id/product_family",
		"/sys/class/dmi/id/board_name",
	} {
		if b, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return "PC"
}
