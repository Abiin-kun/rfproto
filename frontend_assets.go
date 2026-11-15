package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// frontendObserver is a read-only passenger on the authenticated frame stream.
// It never replies and never changes the protocol: it only caches the wallpaper
// and device status the phone already pushes after pairing.
type frontendObserver struct {
	dir string
	mu  sync.Mutex

	phone            phoneProfile
	wallpaperPath    string
	wallpaperVersion uint64
	wallpaperBytes   int
	wallpaperWidth   int
	wallpaperHeight  int
	wallpaperMD5     string
	version          uint64
	pairingQR        string

	wallpaper chan []byte
}

type phoneProfile struct {
	Model          string `json:"model"`
	Battery        int    `json:"battery"`
	HasBattery     bool   `json:"hasBattery"`
	Charging       bool   `json:"charging"`
	Wifi           bool   `json:"wifi"`
	WifiSignal     int    `json:"wifiSignal"`
	MobileData     bool   `json:"mobileData"`
	Bluetooth      bool   `json:"bluetooth"`
	Screen         bool   `json:"screen"`
	WallpaperState int    `json:"wallpaperState"`
	USB            bool   `json:"usb"`
	Version        string `json:"version"`
}

type frontendSnapshot struct {
	Phone            phoneProfile
	WallpaperPath    string
	WallpaperVersion uint64
	WallpaperBytes   int
	WallpaperWidth   int
	WallpaperHeight  int
	Version          uint64
	PairingQR        string `json:"-"`
}

func newFrontendObserver(dir string) *frontendObserver {
	f := &frontendObserver{dir: dir, wallpaper: make(chan []byte, 1)}
	f.wallpaperPath = filepath.Join(dir, "wallpaper.png")
	if info, err := os.Stat(f.wallpaperPath); err == nil {
		if file, err := os.Open(f.wallpaperPath); err == nil {
			if cfg, err := png.DecodeConfig(file); err == nil {
				f.wallpaperBytes = int(info.Size())
				f.wallpaperWidth, f.wallpaperHeight = cfg.Width, cfg.Height
				f.wallpaperVersion = 1
				f.version = 1
				log.Printf("[frontend] cached wallpaper %dx%d (%d bytes)", cfg.Width, cfg.Height, info.Size())
			}
			file.Close()
		}
	}
	go f.wallpaperWorker()
	return f
}

func (f *frontendObserver) Snapshot() frontendSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return frontendSnapshot{
		Phone: f.phone, WallpaperPath: f.wallpaperPath, WallpaperVersion: f.wallpaperVersion,
		WallpaperBytes: f.wallpaperBytes, WallpaperWidth: f.wallpaperWidth,
		WallpaperHeight: f.wallpaperHeight, Version: f.version,
		PairingQR: f.pairingQR,
	}
}

func (f *frontendObserver) setPairingQR(payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairingQR = payload
	f.version++
}
func (f *frontendObserver) IconPath(pkg string) string {
	name := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(pkg)
	if name == "" || name == "." {
		return ""
	}
	return filepath.Join(f.dir, "icons", name+".png")
}

// Observe runs on the WebSocket receive goroutine. Only the small device-status
// frame is parsed inline; a wallpaper decode is handed to a single worker so a
// 700 KB push can never stall the phone's control channel.
func (f *frontendObserver) Observe(service int, msg []byte) {
	if f == nil || service != 25 {
		return
	}
	var frame struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(msg, &frame) != nil || len(frame.Data) == 0 {
		return
	}
	var data struct {
		Cmd            *int   `json:"cmd"`
		Model          string `json:"model"`
		DeviceName     string `json:"deviceName"`
		Battery        *int   `json:"battery"`
		Charging       bool   `json:"isCharging"`
		Wifi           bool   `json:"wifi"`
		WifiSignal     int    `json:"wifiSignal"`
		Data           bool   `json:"data"`
		DataSignal     int    `json:"dataSignal"`
		Bluetooth      bool   `json:"bluetooth"`
		Screen         bool   `json:"screen"`
		WallpaperState int    `json:"wallpaperState"`
		USB            bool   `json:"usbReady"`
		Version        string `json:"ver"`
		Wallpaper      string `json:"wallpaper"`
	}
	if json.Unmarshal(frame.Data, &data) != nil || data.Cmd == nil {
		return
	}
	switch *data.Cmd {
	case 1:
		profile := phoneProfile{
			Model: data.Model, Charging: data.Charging, Wifi: data.Wifi, WifiSignal: data.WifiSignal,
			MobileData: data.Data, Bluetooth: data.Bluetooth, Screen: data.Screen,
			WallpaperState: data.WallpaperState, USB: data.USB, Version: data.Version,
		}
		if profile.Model == "" {
			profile.Model = data.DeviceName
		}
		if data.Battery != nil {
			profile.Battery, profile.HasBattery = *data.Battery, true
		}
		f.mu.Lock()
		if f.phone != profile {
			f.phone = profile
			f.version++
		}
		f.mu.Unlock()
	case 2:
		if data.Wallpaper == "" {
			return
		}
		select {
		case f.wallpaper <- msg:
		default:
			log.Printf("[frontend] wallpaper update skipped: the previous one is still being written")
		}
	}
}

func (f *frontendObserver) wallpaperWorker() {
	for msg := range f.wallpaper {
		var frame struct {
			Data struct {
				MD5       string `json:"md5"`
				Wallpaper string `json:"wallpaper"`
			} `json:"data"`
		}
		if json.Unmarshal(msg, &frame) != nil || frame.Data.Wallpaper == "" {
			continue
		}
		if err := f.saveWallpaper(frame.Data.Wallpaper, frame.Data.MD5); err != nil {
			log.Printf("[frontend] wallpaper: %v", err)
		}
	}
}

func (f *frontendObserver) saveWallpaper(encoded, advertisedMD5 string) error {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if cfg.Width < 64 || cfg.Height < 64 || cfg.Width > 8192 || cfg.Height > 8192 {
		return errInvalidWallpaper
	}
	sum := md5.Sum(raw)
	digest := hex.EncodeToString(sum[:])
	if advertisedMD5 == "" {
		advertisedMD5 = digest
	}
	f.mu.Lock()
	same := f.wallpaperMD5 == digest && f.wallpaperVersion > 0
	f.mu.Unlock()
	if same {
		return nil
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(f.dir, ".wallpaper-*.png")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, f.wallpaperPath); err != nil {
		return err
	}
	f.mu.Lock()
	f.wallpaperMD5 = digest
	f.wallpaperBytes = len(raw)
	f.wallpaperWidth, f.wallpaperHeight = cfg.Width, cfg.Height
	f.wallpaperVersion++
	f.version++
	version := f.wallpaperVersion
	f.mu.Unlock()
	log.Printf("[frontend] wallpaper %dx%d (%d bytes) cached as %s (v%d)", cfg.Width, cfg.Height, len(raw), f.wallpaperPath, version)
	return nil
}

// frontendObserverHook is installed once by serve. nil keeps headless and test
// runs from doing any extra decoding.
var frontendObserverHook func(service int, msg []byte)

func observeFrontendFrame(service int, msg []byte) {
	if frontendObserverHook != nil {
		frontendObserverHook(service, msg)
	}
}

type wallpaperError string

func (e wallpaperError) Error() string { return string(e) }

const errInvalidWallpaper = wallpaperError("phone supplied an unsupported wallpaper image")

// wallpaperModTime is used only for the CLI's cache line.
func (f *frontendObserver) wallpaperModTime() time.Time {
	info, err := os.Stat(f.wallpaperPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
