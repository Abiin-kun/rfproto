package main

/*
#cgo pkg-config: sdl3 freetype2
#cgo LDFLAGS: -lm
#include "native_ui.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	_ "embed"
	"fmt"
	"hash/fnv"
	"image"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/InterVariable.ttf
var nativeUIFont []byte

const (
	nativeWeightRegular  = 400
	nativeWeightMedium   = 500
	nativeWeightSemibold = 600
)

const (
	colGround   = 0x000000
	colSurface  = 0x111111
	colRaised   = 0x2a2a2a
	colBorder   = 0x1a1a1a
	colRule     = 0x161616
	colControl  = 0x444444
	colText     = 0xffffff
	colMuted    = 0xb4b4b4
	colFaint    = 0x949494
	colDim      = 0x6f6f6f
	colAccent   = 0xc6f24e
	colNegative = 0xe5484d
	colWarning  = 0xe5a93b
)

const (
	orientAuto = iota
	orientLandscape
	orientPortrait
)

var orientationModes = [...]string{"auto", "landscape", "portrait"}

type nativeRect struct{ x, y, w, h float32 }

func (r nativeRect) contains(x, y float32) bool {
	return x >= r.x && y >= r.y && x < r.x+r.w && y < r.y+r.h
}
func (r nativeRect) inset(n float32) nativeRect {
	return nativeRect{r.x + n, r.y + n, max(float32(0), r.w-2*n), max(float32(0), r.h-2*n)}
}
func (r nativeRect) center() (float32, float32) { return r.x + r.w/2, r.y + r.h/2 }

type nativeTextKey struct {
	text   string
	size   int
	weight int
}
type nativeSize struct{ w, h float32 }
type nativeCachedText struct {
	texture *C.native_text
	w, h    float32
	used    uint64
}

// nativeCanvas owns one window's renderer-side caches. Text and texture
// objects belong to that window's renderer, so nothing is shared between the
// launcher and the app windows.
type nativeCanvas struct {
	win       *C.native_window
	assets    *frontendObserver
	width     int
	height    int
	density   float32
	texts     map[nativeTextKey]*nativeCachedText
	measures  map[nativeTextKey]nativeSize
	icons     map[string]*C.native_texture
	tried     map[string]bool
	wall      *C.native_texture
	wallVer   uint64
	wallW     int
	wallH     int
	frame     uint64
	qr        *C.native_texture
	qrText    string
	qrModules int
}

func newNativeCanvas(win *C.native_window, assets *frontendObserver) *nativeCanvas {
	return &nativeCanvas{win: win, assets: assets, texts: make(map[nativeTextKey]*nativeCachedText),
		measures: make(map[nativeTextKey]nativeSize), icons: make(map[string]*C.native_texture),
		tried: make(map[string]bool)}
}

func (c *nativeCanvas) refresh() {
	var w, h C.int
	var d C.float
	if C.nui_window_get_size(c.win, &w, &h, &d) == 0 {
		return
	}
	c.width, c.height = int(w), int(h)
	density := float32(d)
	if density <= 0 {
		density = 1
	}
	if density != c.density {
		c.clearTexts()
		c.density = density
	}
}

func (c *nativeCanvas) begin() {
	C.nui_begin(c.win)
	c.frame++
}

func (c *nativeCanvas) present() { C.nui_present(c.win) }

func (c *nativeCanvas) clearTexts() {
	for key, text := range c.texts {
		C.nui_text_free(text.texture)
		delete(c.texts, key)
	}
	clear(c.measures)
}

func (c *nativeCanvas) close() {
	c.clearTexts()
	for key, texture := range c.icons {
		C.nui_texture_free(texture)
		delete(c.icons, key)
	}
	if c.wall != nil {
		C.nui_texture_free(c.wall)
		c.wall = nil
	}
	if c.qr != nil {
		C.nui_texture_free(c.qr)
		c.qr = nil
	}
}

func (c *nativeCanvas) textValue(text string, size, weight int) *nativeCachedText {
	if text == "" {
		return nil
	}
	key := nativeTextKey{text, size, weight}
	if cached := c.texts[key]; cached != nil {
		cached.used = c.frame
		return cached
	}
	if len(c.texts) >= 2048 {
		var oldest nativeTextKey
		age := ^uint64(0)
		for k, cached := range c.texts {
			if cached.used < age {
				oldest, age = k, cached.used
			}
		}
		C.nui_text_free(c.texts[oldest].texture)
		delete(c.texts, oldest)
	}
	cs := C.CString(text)
	texture := C.nui_text_new(c.win, cs, C.int(size), C.int(weight), C.float(c.density))
	C.free(unsafe.Pointer(cs))
	if texture == nil {
		return nil
	}
	var w, h C.float
	C.nui_text_metrics(texture, &w, &h)
	cached := &nativeCachedText{texture: texture, w: float32(w), h: float32(h), used: c.frame}
	c.texts[key] = cached
	return cached
}

func (c *nativeCanvas) label(text string, size, weight int, x, y float32, color uint32) {
	if cached := c.textValue(text, size, weight); cached != nil {
		C.nui_text_draw(c.win, cached.texture, C.float(x), C.float(y), C.uint(color))
	}
}

func (c *nativeCanvas) labelAlpha(text string, size, weight int, x, y float32, color, alpha uint32) {
	if cached := c.textValue(text, size, weight); cached != nil {
		C.nui_text_draw_a(c.win, cached.texture, C.float(x), C.float(y), C.uint(color), C.uint(alpha))
	}
}

func (c *nativeCanvas) labelCentered(text string, size, weight int, r nativeRect, color uint32) {
	cached := c.textValue(text, size, weight)
	if cached == nil {
		return
	}
	C.nui_text_draw(c.win, cached.texture, C.float(r.x+(r.w-cached.w)/2), C.float(r.y+(r.h-cached.h)/2), C.uint(color))
}

func (c *nativeCanvas) labelRight(text string, size, weight int, right, y float32, color uint32) {
	if cached := c.textValue(text, size, weight); cached != nil {
		C.nui_text_draw(c.win, cached.texture, C.float(right-cached.w), C.float(y), C.uint(color))
	}
}

func (c *nativeCanvas) measure(text string, size, weight int) nativeSize {
	if text == "" {
		return nativeSize{}
	}
	key := nativeTextKey{text, size, weight}
	if got, ok := c.measures[key]; ok {
		return got
	}
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	var w, h C.float
	C.nui_text_measure(c.win, cs, C.int(size), C.int(weight), C.float(c.density), &w, &h)
	got := nativeSize{float32(w), float32(h)}
	c.measures[key] = got
	return got
}

func (c *nativeCanvas) textWidth(text string, size, weight int) float32 {
	return c.measure(text, size, weight).w
}

func (c *nativeCanvas) fill(r nativeRect, color uint32) {
	if r.w <= 0 || r.h <= 0 {
		return
	}
	C.nui_fill(c.win, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.uint(color))
}
func (c *nativeCanvas) fillAlpha(r nativeRect, color, alpha uint32) {
	if r.w <= 0 || r.h <= 0 || alpha == 0 {
		return
	}
	C.nui_fill_a(c.win, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.uint(color), C.uint(alpha))
}
func (c *nativeCanvas) round(r nativeRect, radius float32, color uint32) {
	if r.w <= 0 || r.h <= 0 {
		return
	}
	C.nui_round(c.win, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.float(radius), C.uint(color))
}
func (c *nativeCanvas) roundAlpha(r nativeRect, radius float32, color, alpha uint32) {
	if r.w <= 0 || r.h <= 0 || alpha == 0 {
		return
	}
	C.nui_round_a(c.win, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.float(radius), C.uint(color), C.uint(alpha))
}
func (c *nativeCanvas) border(r nativeRect, radius, width float32, color, inner uint32) {
	c.round(nativeRect{r.x - width, r.y - width, r.w + width*2, r.h + width*2}, radius+width, color)
	c.round(r, radius, inner)
}
func (c *nativeCanvas) clip(r nativeRect) {
	C.nui_clip(c.win, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h))
}
func (c *nativeCanvas) unclip() { C.nui_unclip(c.win) }

// clipRunes truncates by the approximate advance width of Inter. It avoids
// expensive per-rune measurements in a 60 Hz grid.
func clipRunes(text string, size int, width float32) string {
	if text == "" || width <= 0 {
		return ""
	}
	limit := int(width / (float32(size) * 0.55))
	if limit < 1 {
		limit = 1
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func trimRunes(text string, keep int) string {
	if keep < 1 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= keep {
		return text
	}
	return string(runes[len(runes)-keep:])
}

func monogram(name string) string {
	var letters []rune
	fields := strings.Fields(name)
	for _, field := range fields {
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				letters = append(letters, unicode.ToUpper(r))
				break
			}
		}
		if len(letters) == 2 {
			break
		}
	}
	if len(letters) == 0 {
		for _, r := range name {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				letters = append(letters, unicode.ToUpper(r))
				break
			}
		}
	}
	if len(letters) == 0 {
		return "·"
	}
	return string(letters)
}

func identityHue(seed string) float64 {
	hash := fnv.New32a()
	hash.Write([]byte(seed))
	return float64(hash.Sum32() % 360)
}

func hslColor(h, s, l float64) uint32 {
	h = math.Mod(h, 360)
	if h < 0 {
		h += 360
	}
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	to := func(v float64) uint32 {
		n := int(math.Round((v + m) * 255))
		if n < 0 {
			n = 0
		}
		if n > 255 {
			n = 255
		}
		return uint32(n)
	}
	return to(r)<<16 | to(g)<<8 | to(b)
}

func appTint(seed string) uint32 {
	// Identity, not data: desaturated so a grid of 200 marks never becomes a
	// rainbow next to the one live-status lime.
	return hslColor(identityHue(seed), 0.34, 0.29)
}

func (c *nativeCanvas) iconTexture(pkg string) *C.native_texture {
	if pkg == "" || c.assets == nil {
		return nil
	}
	if c.tried[pkg] {
		return c.icons[pkg]
	}
	c.tried[pkg] = true
	path := c.assets.IconPath(pkg)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	img, _, err := image.Decode(strings.NewReader(string(data)))
	if err != nil {
		return nil
	}
	texture := c.uploadIcon(img, 96)
	if texture != nil {
		c.icons[pkg] = texture
	}
	return texture
}

func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	return rgba
}

func resizeIcon(img image.Image, size int) *image.RGBA {
	src := toRGBA(img)
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw <= 0 || sh <= 0 {
		return out
	}
	for y := 0; y < size; y++ {
		sy := y * sh / size
		for x := 0; x < size; x++ {
			sx := x * sw / size
			copy(out.Pix[(y*out.Stride+x*4):(y*out.Stride+x*4+4)], src.Pix[sy*src.Stride+sx*4:sy*src.Stride+sx*4+4])
		}
	}
	return out
}

func (c *nativeCanvas) uploadIcon(img image.Image, size int) *C.native_texture {
	rgba := resizeIcon(img, size)
	texture := C.nui_texture_rgba(c.win, unsafe.Pointer(&rgba.Pix[0]), C.int(size), C.int(size))
	if texture == nil {
		return nil
	}
	return texture
}

func (c *nativeCanvas) appIcon(app viewerApp, r nativeRect) {
	if texture := c.iconTexture(app.Package); texture != nil {
		C.nui_texture_cover_round(c.win, texture, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.float(r.w*0.27), C.uint(255))
		return
	}
	tint := appTint(app.Package)
	c.round(r, r.w*0.27, tint)
	size := int(r.w * 0.42)
	c.labelCentered(monogram(app.Name), size, nativeWeightSemibold, nativeRect{r.x, r.y - 1, r.w, r.h}, 0xffffff)
}

func (c *nativeCanvas) genericIcon(r nativeRect) {
	tint := appTint("rfproto")
	c.round(r, r.w*0.27, tint)
	c.labelCentered("rf", int(r.w*0.38), nativeWeightSemibold, nativeRect{r.x, r.y - 1, r.w, r.h}, 0xffffff)
}

func (c *nativeCanvas) syncPairingQR(snapshot frontendSnapshot) {
	if snapshot.PairingQR == c.qrText {
		return
	}
	if c.qr != nil {
		C.nui_texture_free(c.qr)
		c.qr = nil
	}
	c.qrText = snapshot.PairingQR
	if c.qrText == "" {
		return
	}
	code, err := qrcode.New(c.qrText, qrcode.Medium)
	if err != nil {
		log.Printf("[ui] pairing QR: %v", err)
		return
	}
	rgba := toRGBA(code.Image(-1))
	c.qrModules = rgba.Bounds().Dx()
	c.qr = C.nui_texture_rgba(c.win, unsafe.Pointer(&rgba.Pix[0]), C.int(c.qrModules), C.int(c.qrModules))
	C.nui_texture_nearest(c.qr)
}

func (c *nativeCanvas) syncWallpaper(snapshot frontendSnapshot) {
	if snapshot.WallpaperVersion == 0 || snapshot.WallpaperVersion == c.wallVer {
		return
	}
	path := snapshot.WallpaperPath
	if path == "" {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		c.wallVer = snapshot.WallpaperVersion
		return
	}
	img, err := png.Decode(file)
	file.Close()
	if err != nil {
		c.wallVer = snapshot.WallpaperVersion
		log.Printf("[frontend] wallpaper decode: %v", err)
		return
	}
	rgba := toRGBA(img)
	if c.wall != nil {
		C.nui_texture_free(c.wall)
	}
	c.wall = C.nui_texture_rgba(c.win, unsafe.Pointer(&rgba.Pix[0]), C.int(rgba.Bounds().Dx()), C.int(rgba.Bounds().Dy()))
	c.wallVer = snapshot.WallpaperVersion
	c.wallW, c.wallH = rgba.Bounds().Dx(), rgba.Bounds().Dy()
	if c.wall == nil {
		log.Printf("[frontend] wallpaper upload failed: %s", C.GoString(C.SDL_GetError()))
	}
}

func (c *nativeCanvas) wallpaper(r nativeRect, radius float32, alpha uint32) bool {
	if c.wall == nil {
		return false
	}
	C.nui_texture_cover_round(c.win, c.wall, C.float(r.x), C.float(r.y), C.float(r.w), C.float(r.h), C.float(radius), C.uint(alpha))
	return true
}

type launcherHit struct {
	id      string
	region  string
	rect    nativeRect
	enabled bool
	app     int
	session string
}

type nativeLauncher struct {
	nativeCanvas
	front *frontend

	focused     bool
	searchFocus bool
	textOn      bool
	textRect    nativeRect
	search      string

	apps     []viewerApp
	selected int
	scroll   float32
	hover    string
	mouseX   float32
	mouseY   float32
	hits     []launcherHit
	toast    string
	toastBad bool
	toastEnd time.Time
	caret    time.Time

	snapshot   frontendSnapshot
	sizeLarge  bool
	muted      bool
	railW      float32
	phoneW     float32
	topbarH    float32
	footerH    float32
	mainX      float32
	mainRight  float32
	grid       nativeRect
	searchRect nativeRect
	phone      nativeRect
	sessionAt  []nativeRect
	sessionIDs []string
	desktop    nativeRect
	sizeSeg    nativeRect
	sound      nativeRect
	footer     nativeRect
	trayButton nativeRect
	cols       int
	tileW      float32
	tileH      float32
	rowH       float32
	gap        float32
	maxScroll  float32
}

func (l *nativeLauncher) layout() {
	w, h := float32(l.width), float32(l.height)
	if w < 2 || h < 2 {
		return
	}
	l.hits = l.hits[:0]
	l.sessionAt = l.sessionAt[:0]
	l.sessionIDs = l.sessionIDs[:0]

	l.railW = 228
	if w < 1180 {
		l.railW = 208
	}
	switch {
	case w >= 1320:
		l.phoneW = 300
	case w >= 1120:
		l.phoneW = 272
	default:
		l.phoneW = 240
	}
	l.topbarH = 64
	l.footerH = 36
	l.mainX = l.railW + 1
	l.mainRight = w - l.phoneW - 1
	contentW := l.mainRight - l.mainX

	l.searchRect = nativeRect{l.mainRight - 26 - min(300, contentW*0.44), 14, min(300, contentW*0.44), 36}
	l.grid = nativeRect{l.mainX + 26, l.topbarH + 22, contentW - 52, h - l.topbarH - l.footerH - 28}
	l.footer = nativeRect{l.mainX, h - l.footerH, w - l.mainX, l.footerH}
	l.phone = nativeRect{l.mainRight + 1, 0, l.phoneW, h}

	pad := float32(20)
	l.desktop = nativeRect{pad, 188, l.railW - pad*2, 38}
	l.sizeSeg = nativeRect{pad, 254, l.railW - pad*2, 32}
	l.sound = nativeRect{l.railW - pad - 36, 298, 36, 20}
	l.trayButton = nativeRect{pad, h - 52, l.railW - pad*2, 34}

	l.hits = append(l.hits,
		launcherHit{id: "search", region: "search", rect: l.searchRect, enabled: true},
		launcherHit{id: "desktop", region: "rail", rect: l.desktop, enabled: l.front.state.Connected},
		launcherHit{id: "size720", region: "rail", rect: nativeRect{l.sizeSeg.x, l.sizeSeg.y, (l.sizeSeg.w - 4) / 2, l.sizeSeg.h}, enabled: true},
		launcherHit{id: "size1080", region: "rail", rect: nativeRect{l.sizeSeg.x + (l.sizeSeg.w-4)/2 + 4, l.sizeSeg.y, (l.sizeSeg.w - 4) / 2, l.sizeSeg.h}, enabled: true},
		launcherHit{id: "sound", region: "rail", rect: l.sound, enabled: true},
		launcherHit{id: "tray", region: "rail", rect: l.trayButton, enabled: true},
	)

	l.gap = 10
	l.tileH = 112
	l.rowH = 120
	minTile := float32(154)
	l.cols = int((l.grid.w + l.gap) / (minTile + l.gap))
	if l.cols < 2 {
		l.cols = 2
	}
	l.tileW = (l.grid.w - l.gap*float32(l.cols-1)) / float32(l.cols)
	if l.selected >= len(l.apps) {
		l.selected = len(l.apps) - 1
	}
	if l.selected < 0 && len(l.apps) > 0 {
		l.selected = 0
	}
	rows := (len(l.apps) + l.cols - 1) / l.cols
	total := float32(rows) * l.rowH
	l.maxScroll = max(0, total-l.grid.h+l.rowH-l.tileH)
	// Wheel scrolling must not snap back to the keyboard selection each frame.
	l.scroll = max(0, min(l.scroll, l.maxScroll))

	start := l.grid.y - l.scroll
	for i := range l.apps {
		row := i / l.cols
		col := i % l.cols
		x := l.grid.x + float32(col)*(l.tileW+l.gap)
		y := start + float32(row)*l.rowH
		if y+l.tileH < l.grid.y || y > l.grid.y+l.grid.h {
			continue
		}
		l.hits = append(l.hits, launcherHit{id: fmt.Sprintf("app:%d", i), region: "grid", rect: nativeRect{x, y, l.tileW, l.tileH}, enabled: l.front.state.Connected, app: i})
	}

	// Open displays live in the rail; cap them where the footer begins.
	sessionTop := float32(392)
	sessionBottom := h - 58
	maxRows := int((sessionBottom - sessionTop) / 42)
	if maxRows < 1 {
		maxRows = 1
	}
	if maxRows > 4 {
		maxRows = 4
	}
	for i, session := range l.front.state.Sessions {
		if i >= maxRows {
			break
		}
		rect := nativeRect{8, sessionTop + float32(i)*42, l.railW - 16, 40}
		l.sessionAt = append(l.sessionAt, rect)
		l.sessionIDs = append(l.sessionIDs, session.ID)
		l.hits = append(l.hits, launcherHit{id: "session:" + session.ID, region: "rail", rect: rect, enabled: true, session: session.ID})
	}

	l.hover = l.hitAt(l.mouseX, l.mouseY)
}

func (l *nativeLauncher) ensureVisible() {
	if l.selected < 0 || len(l.apps) == 0 || l.cols < 1 {
		l.scroll = 0
		return
	}
	row := float32(l.selected / l.cols)
	top := row * l.rowH
	bottom := top + l.tileH
	if top < l.scroll {
		l.scroll = top
	}
	if bottom > l.scroll+l.grid.h {
		l.scroll = bottom - l.grid.h
	}
	l.scroll = max(0, min(l.scroll, l.maxScroll))
}

func (l *nativeLauncher) hitAt(x, y float32) string {
	for _, hit := range l.hits {
		if !hit.enabled || !hit.rect.contains(x, y) {
			continue
		}
		if hit.region == "grid" && !l.grid.contains(x, y) {
			continue
		}
		return hit.id
	}
	return ""
}

func (l *nativeLauncher) hit(id string) (launcherHit, bool) {
	for _, h := range l.hits {
		if h.id == id {
			return h, true
		}
	}
	return launcherHit{}, false
}

func (l *nativeLauncher) filter() {
	selectedID := ""
	if l.selected >= 0 && l.selected < len(l.apps) {
		selectedID = l.apps[l.selected].ID
	}
	query := strings.ToLower(strings.TrimSpace(l.search))
	l.apps = l.apps[:0]
	for _, app := range l.front.state.Apps {
		if query != "" && !strings.Contains(strings.ToLower(app.Name), query) && !strings.Contains(strings.ToLower(app.Package), query) {
			continue
		}
		l.apps = append(l.apps, app)
	}
	l.selected = -1
	if selectedID != "" {
		for i, app := range l.apps {
			if app.ID == selectedID {
				l.selected = i
				break
			}
		}
	}
	if l.selected < 0 && len(l.apps) > 0 {
		l.selected = 0
	}
	if l.selected >= len(l.apps) {
		l.selected = len(l.apps) - 1
	}
	l.ensureVisible()
}

func (l *nativeLauncher) toastMessage(text string, bad bool) {
	l.toast, l.toastBad = text, bad
	l.toastEnd = time.Now().Add(4 * time.Second)
}

func (l *nativeLauncher) setSearchFocus(on bool) {
	if l.searchFocus == on {
		return
	}
	l.searchFocus = on
	l.updateTextInput()
}

func (l *nativeLauncher) updateTextInput() {
	on := l.focused && l.searchFocus
	if on == l.textOn && (!on || l.textRect == l.searchRect) {
		return
	}
	enabled := C.int(0)
	if on {
		enabled = 1
	}
	C.nui_text_input(l.nativeCanvas.win, enabled, C.float(l.searchRect.x), C.float(l.searchRect.y), C.float(l.searchRect.w), C.float(l.searchRect.h))
	l.textOn, l.textRect = on, l.searchRect
}

func (l *nativeLauncher) focusedEvent(on bool) {
	l.focused = on
	if !on {
		l.textOn = false
		C.nui_text_input(l.nativeCanvas.win, 0, 0, 0, 0, 0)
		return
	}
	l.updateTextInput()
}

func (l *nativeLauncher) openSession(id string) {
	l.front.focusSession(id)
}

func (l *nativeLauncher) appForName(name string) string {
	for _, app := range l.front.state.Apps {
		if strings.EqualFold(app.Name, name) {
			return app.Package
		}
	}
	return ""
}

func (l *nativeLauncher) sessionOpen(name string) bool {
	for _, s := range l.front.state.Sessions {
		if s.Mode == "app" && strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

func (l *nativeLauncher) launchApp(index int) {
	if index < 0 || index >= len(l.apps) {
		return
	}
	app := l.apps[index]
	l.launch("app", app.ID, app.Name)
}

func (l *nativeLauncher) launch(mode, appID, name string) {
	if !l.front.state.Connected {
		l.toastMessage("Connect the paired phone first", true)
		return
	}
	width, height := 1280, 720
	if l.sizeLarge {
		width, height = 1920, 1080
	}
	result, err := l.front.hub.command(viewerCommand{Op: "launch", Mode: mode, AppID: appID, Width: width, Height: height})
	if err != nil {
		l.front.setError(err.Error())
		return
	}
	reply, _ := result.(map[string]string)
	id := reply["sessionId"]
	if id != "" {
		l.front.pendingFocus = id
	}
	if mode == "desktop" {
		l.toastMessage("Opening the mobile desktop", false)
	} else {
		l.toastMessage("Opening "+name, false)
	}
	l.front.setError("")
	l.front.applyState(l.front.hub.state())
	l.front.syncStreams()
	if id != "" {
		l.front.focusSession(id)
	}
}

func (l *nativeLauncher) toggleMute() {
	l.front.setMuted(!l.muted)
	if l.muted {
		l.toastMessage("Phone audio muted", false)
	} else {
		l.toastMessage("Phone audio on", false)
	}
}

func (l *nativeLauncher) draw() {
	l.begin()
	w, h := float32(l.width), float32(l.height)
	l.fill(nativeRect{0, 0, w, h}, colGround)

	// One ground for rail, bar and page; separation is a single hairline.
	l.fill(nativeRect{l.railW, 0, 1, h}, colRule)
	l.fill(nativeRect{l.mainX, l.topbarH, l.mainRight - l.mainX, 1}, colRule)
	l.fill(nativeRect{l.mainRight, 0, 1, h}, colRule)
	l.fill(nativeRect{l.mainX, h - l.footerH, w - l.mainX, 1}, colRule)

	l.drawRail()
	l.drawTopbar()
	l.drawGrid()
	l.drawPhone()
	l.drawFooter()
	l.drawToast()
	l.present()
}

func (l *nativeLauncher) drawRail() {
	pad := float32(20)
	h := float32(l.height)
	l.label("rfproto", 18, nativeWeightSemibold, pad, 18, colText)
	l.fill(nativeRect{pad, 64, l.railW - pad*2, 1}, colRule)

	phone := l.snapshot.Phone
	model := phone.Model
	if model == "" {
		model = l.front.state.Phone
	}
	if model == "" {
		model = "Not connected"
	}
	l.label("PHONE", 10, nativeWeightSemibold, pad, 82, colDim)
	l.label(clipRunes(model, 15, l.railW-pad*2), 15, nativeWeightSemibold, pad, 100, colText)

	connected := l.front.state.Connected
	dot := uint32(colControl)
	status := "Waiting for a phone"
	statusColor := uint32(colMuted)
	if connected {
		dot = colAccent
		status = "Connected"
		statusColor = colText
	}
	l.round(nativeRect{pad + 2, 126, 6, 6}, 3, dot)
	l.label(status, 11, nativeWeightMedium, pad+14, 120, statusColor)

	if phone.HasBattery {
		battery := fmt.Sprintf("%d%%", phone.Battery)
		batteryColor := uint32(colText)
		if phone.Battery <= 20 && !phone.Charging {
			batteryColor = colWarning
		}
		l.label(battery, 15, nativeWeightMedium, pad, 142, batteryColor)
		detail := "battery"
		if phone.Charging {
			detail = "battery · charging"
		}
		l.label(detail, 10, nativeWeightRegular, pad+l.textWidth(battery, 15, nativeWeightMedium)+8, 147, colFaint)
	} else {
		l.label("Battery", 11, nativeWeightRegular, pad, 144, colDim)
	}
	network := "No network"
	if phone.Wifi {
		network = fmt.Sprintf("Wi-Fi · signal %d/4", phone.WifiSignal)
	} else if phone.MobileData {
		network = "Mobile data"
	}
	l.label(network, 10, nativeWeightRegular, pad, 164, colFaint)

	l.round(l.desktop, 999, colSurface)
	if connected {
		color := uint32(0xf2f2f2)
		if l.hover == "desktop" {
			color = 0xffffff
		}
		l.round(l.desktop, 999, color)
		l.labelCentered("Open mobile desktop", 12, nativeWeightMedium, l.desktop, 0x000000)
	} else {
		l.labelCentered("Open mobile desktop", 12, nativeWeightMedium, l.desktop, colDim)
	}

	l.label("DISPLAY", 10, nativeWeightSemibold, pad, 238, colDim)
	l.round(l.sizeSeg, 999, colSurface)
	half := (l.sizeSeg.w - 4) / 2
	left := nativeRect{l.sizeSeg.x + 2, l.sizeSeg.y + 2, half, l.sizeSeg.h - 4}
	right := nativeRect{l.sizeSeg.x + half + 2, l.sizeSeg.y + 2, half, l.sizeSeg.h - 4}
	if !l.sizeLarge {
		l.round(left, 999, 0x242424)
	}
	if l.sizeLarge {
		l.round(right, 999, 0x242424)
	}
	color720 := uint32(colText)
	color1080 := uint32(colMuted)
	if l.sizeLarge {
		color720, color1080 = colMuted, colText
	}
	l.labelCentered("720p", 11, nativeWeightMedium, left, color720)
	l.labelCentered("1080p", 11, nativeWeightMedium, right, color1080)

	l.label("Sound", 12, nativeWeightRegular, pad, 300, colMuted)
	l.round(l.sound, 999, colRaised)
	knobX := l.sound.x + 2
	if !l.muted {
		knobX = l.sound.x + l.sound.w - 18
		l.round(nativeRect{l.sound.x + 2, l.sound.y + 2, 30, l.sound.h - 4}, 999, 0x2b2b2b)
	} else {
		l.round(nativeRect{l.sound.x + 2, l.sound.y + 2, 30, l.sound.h - 4}, 999, colSurface)
	}
	knobColor := uint32(0x777777)
	if !l.muted {
		knobColor = colText
	}
	l.round(nativeRect{knobX, l.sound.y + 2, 16, 16}, 8, knobColor)

	if h >= 710 {
		l.label("Wallpaper", 12, nativeWeightRegular, pad, 330, colMuted)
		value := "Waiting"
		if l.nativeCanvas.wall != nil {
			value = "From phone"
		}
		l.labelRight(value, 10, nativeWeightRegular, l.railW-pad, 332, colDim)
	}

	l.fill(nativeRect{pad, 360, l.railW - pad*2, 1}, colRule)
	l.label("OPEN DISPLAYS", 10, nativeWeightSemibold, pad, 378, colDim)
	l.labelRight(fmt.Sprintf("%d", len(l.front.state.Sessions)), 10, nativeWeightMedium, l.railW-pad, 378, colMuted)

	if len(l.front.state.Sessions) == 0 {
		l.label("No app windows open", 11, nativeWeightRegular, pad, 404, colDim)
	}
	for i, rect := range l.sessionAt {
		if i >= len(l.front.state.Sessions) {
			break
		}
		s := l.front.state.Sessions[i]
		if l.hover == "session:"+s.ID {
			l.round(rect, 10, colSurface)
		}
		iconRect := nativeRect{rect.x + 10, rect.y + 8, 24, 24}
		l.round(iconRect, 7, appTint(s.Name))
		l.labelCentered(monogram(s.Name), 9, nativeWeightSemibold, nativeRect{iconRect.x, iconRect.y - 1, iconRect.w, iconRect.h}, 0xffffff)
		l.label(clipRunes(s.Name, 12, rect.w-86), 12, nativeWeightMedium, rect.x+42, rect.y+6, colText)
		state := s.State
		color := uint32(colDim)
		switch s.State {
		case "connected":
			state = "Live"
			color = colFaint
		case "requesting":
			state = "Waiting for phone"
		case "negotiating":
			state = "Connecting"
		case "paused":
			state = "Paused"
		case "failed":
			state = "Failed"
			color = colNegative
		}
		l.label(state, 10, nativeWeightRegular, rect.x+42, rect.y+22, color)
		if s.State == "connected" {
			l.round(nativeRect{rect.x + rect.w - 12, rect.y + 17, 5, 5}, 3, colAccent)
		}
	}
	if len(l.front.state.Sessions) > len(l.sessionAt) {
		l.label(fmt.Sprintf("+ %d more", len(l.front.state.Sessions)-len(l.sessionAt)), 10, nativeWeightRegular, pad, 404+float32(len(l.sessionAt))*42, colDim)
	}
	limitY := 404 + float32(max(1, len(l.sessionAt)))*42 + 10
	if limitY < h-84 && l.front.state.AppLimit > 0 {
		l.label(fmt.Sprintf("Up to %d apps at once", l.front.state.AppLimit), 10, nativeWeightRegular, pad, limitY, colDim)
	}
	l.round(l.trayButton, 6, colSurface)
	if l.hover == "tray" {
		l.round(l.trayButton, 6, colRaised)
	}
	l.labelCentered("Send to system tray", 11, nativeWeightMedium, l.trayButton, colMuted)
}

func (l *nativeLauncher) drawTopbar() {
	title := "Apps"
	sub := fmt.Sprintf("%d apps", len(l.front.state.Apps))
	if len(l.front.state.Sessions) > 0 {
		sub += fmt.Sprintf(" · %d open", len(l.front.state.Sessions))
	}
	if l.search != "" {
		sub = fmt.Sprintf("%d results for “%s”", len(l.apps), clipRunes(l.search, 11, 160))
	}
	l.label(title, 20, nativeWeightSemibold, l.mainX+26, 13, colText)
	l.label(sub, 11, nativeWeightRegular, l.mainX+27, 41, colFaint)

	hover := l.hover == "search"
	bg := uint32(colSurface)
	if hover {
		bg = 0x171717
	}
	if l.searchFocus {
		l.border(l.searchRect, 999, 1, colText, bg)
	} else {
		l.round(l.searchRect, 999, bg)
	}
	clip := l.searchRect.inset(1)
	l.clip(clip)
	text := l.search
	color := uint32(colText)
	if text == "" {
		text = "Search apps"
		color = colDim
	}
	text = trimRunes(text, 34)
	tw := l.textWidth(text, 12, nativeWeightRegular)
	l.label(text, 12, nativeWeightRegular, l.searchRect.x+14, l.searchRect.y+10, color)
	if l.searchFocus && time.Now().UnixMilli()/530%2 == 0 {
		x := l.searchRect.x + 14 + tw + 1
		if x < l.searchRect.x+l.searchRect.w-44 {
			l.fill(nativeRect{x, l.searchRect.y + 11, 1, 14}, colText)
		}
	}
	l.unclip()

	chip := "/"
	if l.searchFocus {
		chip = "esc"
	}
	chipW := float32(24)
	if len(chip) > 1 {
		chipW = 34
	}
	chipRect := nativeRect{l.searchRect.x + l.searchRect.w - chipW - 8, l.searchRect.y + 9, chipW, 18}
	l.round(chipRect, 5, 0x1c1c1c)
	l.labelCentered(chip, 9, nativeWeightMedium, chipRect, colMuted)
}

func (l *nativeLauncher) drawGrid() {
	l.clip(l.grid)
	defer l.unclip()

	if !l.front.state.Connected || len(l.apps) == 0 {
		l.drawEmpty()
		return
	}
	start := l.grid.y - l.scroll
	for i, app := range l.apps {
		row := i / l.cols
		col := i % l.cols
		x := l.grid.x + float32(col)*(l.tileW+l.gap)
		y := start + float32(row)*l.rowH
		if y+l.tileH < l.grid.y || y > l.grid.y+l.grid.h {
			continue
		}
		rect := nativeRect{x, y, l.tileW, l.tileH}
		selected := i == l.selected
		hovered := l.hover == fmt.Sprintf("app:%d", i)
		iconSize := float32(58)
		iconRect := nativeRect{rect.x + (rect.w-iconSize)/2, rect.y + 10, iconSize, iconSize}
		if selected {
			l.round(rect, 17, 0x121212)
			l.border(rect, 17, 1, 0x303030, 0x121212)
			l.round(nativeRect{iconRect.x - 2, iconRect.y - 2, iconRect.w + 4, iconRect.h + 4}, iconSize*0.27+2, colText)
		} else if hovered {
			l.round(rect, 17, 0x0e0e0e)
		}
		l.appIcon(app, iconRect)
		if l.sessionOpen(app.Name) {
			l.round(nativeRect{iconRect.x + iconSize - 8, iconRect.y + iconSize - 8, 10, 10}, 5, colGround)
			l.round(nativeRect{iconRect.x + iconSize - 6, iconRect.y + iconSize - 6, 6, 6}, 3, colAccent)
		}
		name := clipRunes(app.Name, 13, rect.w-14)
		nameW := l.textWidth(name, 13, nativeWeightMedium)
		l.label(name, 13, nativeWeightMedium, rect.x+(rect.w-nameW)/2, rect.y+76, colText)
		if selected || hovered {
			pkg := clipRunes(app.Package, 10, rect.w-14)
			pkgW := l.textWidth(pkg, 10, nativeWeightRegular)
			l.label(pkg, 10, nativeWeightRegular, rect.x+(rect.w-pkgW)/2, rect.y+94, 0x8a8a8a)
		}
	}

	rows := (len(l.apps) + l.cols - 1) / l.cols
	total := float32(rows) * l.rowH
	if total > l.grid.h {
		track := nativeRect{l.grid.x + l.grid.w + 7, l.grid.y, 3, l.grid.h}
		l.round(track, 2, 0x0c0c0c)
		thumbH := max(28, l.grid.h*l.grid.h/total)
		thumbY := l.grid.y + (l.grid.h-thumbH)*(l.scroll/l.maxScroll)
		l.round(nativeRect{track.x, thumbY, track.w, thumbH}, 2, colRaised)
	}
}

func (l *nativeLauncher) drawEmpty() {
	x := l.grid.x
	y := l.grid.y + l.grid.h/2 - 70
	if !l.front.state.Connected {
		if l.qr != nil {
			size := min(l.grid.w-24, l.grid.h-110, float32(480))
			size = float32(int(size) / l.qrModules * l.qrModules)
			if size >= float32(l.qrModules) {
				left := l.grid.x + (l.grid.w-size)/2
				top := l.grid.y + 38 + (l.grid.h-size-110)/2
				l.labelCentered("Pair your phone", 20, nativeWeightSemibold,
					nativeRect{l.grid.x, top - 38, l.grid.w, 28}, colText)
				C.nui_texture_draw(l.win, l.qr, C.float(left), C.float(top), C.float(size), C.float(size), 255)
				l.labelCentered("Scan in Ready For / Smart Connect", 12, nativeWeightRegular,
					nativeRect{l.grid.x, top + size + 14, l.grid.w, 22}, colMuted)
				l.labelCentered("Keep this pairing code private.", 11, nativeWeightRegular,
					nativeRect{l.grid.x, top + size + 40, l.grid.w, 20}, colFaint)
				return
			}
		}
		l.label("Waiting for your phone", 20, nativeWeightSemibold, x, y, colText)
		l.label("Pair in Ready For, or reopen the app on your phone.", 12, nativeWeightRegular, x, y+32, colFaint)
		return
	}
	if len(l.front.state.Apps) == 0 {
		l.label("Reading the app list", 20, nativeWeightSemibold, x, y, colText)
		l.label("The phone's launcher decides which apps it shares.", 12, nativeWeightRegular, x, y+32, colFaint)
		l.loading(x, y+62, 180)
		return
	}
	l.label("No apps match", 20, nativeWeightSemibold, x, y, colText)
	l.label("Try a different name or package.", 12, nativeWeightRegular, x, y+32, colFaint)
	l.label("Esc", 10, nativeWeightMedium, x, y+58, colText)
	l.label("clears the search", 11, nativeWeightRegular, x+34, y+57, colDim)
}

func (l *nativeLauncher) loading(x, y, width float32) {
	phase := float32(time.Now().UnixMilli()%1400) / 1400
	l.round(nativeRect{x, y, width, 2}, 1, colRule)
	thumb := float32(48)
	pos := (width + thumb) * phase
	if pos > width {
		pos -= width + thumb
	}
	start := max(float32(0), pos-thumb)
	length := min(thumb, width-start)
	l.round(nativeRect{x + start, y, length, 2}, 1, 0xcfcfcf)
}

func (l *nativeLauncher) drawPhone() {
	pad := float32(22)
	x := l.phone.x + pad
	w := l.phone.w - pad*2
	dot := uint32(colControl)
	state := "Waiting for pairing"
	if l.front.state.Connected {
		dot = colAccent
		state = "Live connection"
	}
	l.round(nativeRect{x + 2, 46, 6, 6}, 3, dot)
	l.label(state, 10, nativeWeightMedium, x+14, 40, colMuted)

	frame := l.phoneFrame()
	l.round(nativeRect{frame.x - 1, frame.y - 1, frame.w + 2, frame.h + 2}, 29, colBorder)
	painted := l.wallpaper(frame, 28, 255)
	if !painted {
		l.round(frame, 28, 0x0b0b0b)
		line1 := "Waiting for"
		line2 := "the phone wallpaper"
		w1 := l.textWidth(line1, 11, nativeWeightRegular)
		w2 := l.textWidth(line2, 11, nativeWeightRegular)
		l.label(line1, 11, nativeWeightRegular, frame.x+(frame.w-w1)/2, frame.y+frame.h/2-18, colDim)
		l.label(line2, 11, nativeWeightRegular, frame.x+(frame.w-w2)/2, frame.y+frame.h/2, colDim)
	}

	phone := l.snapshot.Phone
	if phone.Screen || phone.Model != "" {
		screen := "Screen off"
		screenDot := uint32(colControl)
		if phone.Screen {
			screen = "Screen on"
			screenDot = colAccent
		}
		pillW := l.textWidth(screen, 10, nativeWeightMedium) + 28
		pill := nativeRect{frame.x + 14, frame.y + 14, pillW, 24}
		l.roundAlpha(pill, 999, 0x000000, 190)
		l.round(nativeRect{pill.x + 10, pill.y + 9, 6, 6}, 3, screenDot)
		l.label(screen, 10, nativeWeightMedium, pill.x+22, pill.y+6, colText)
	}

	model := phone.Model
	if model == "" {
		model = l.front.state.Phone
	}
	captionY := frame.y + frame.h + 14
	if model == "" {
		l.label("No phone paired", 12, nativeWeightMedium, x, captionY, colMuted)
	} else {
		l.label(clipRunes(model, 12, w), 12, nativeWeightMedium, x, captionY, colText)
	}
	detail := "Waiting for the phone"
	if phone.HasBattery {
		detail = fmt.Sprintf("%d%% battery", phone.Battery)
		if phone.Charging {
			detail = fmt.Sprintf("%d%% · charging", phone.Battery)
		}
		if phone.Wifi {
			detail += " · Wi-Fi"
		}
	}
	l.label(clipRunes(detail, 10, w), 10, nativeWeightRegular, x, captionY+19, colFaint)
	if l.nativeCanvas.wall != nil {
		meta := fmt.Sprintf("%d × %d wallpaper", l.nativeCanvas.wallW, l.nativeCanvas.wallH)
		l.label(meta, 10, nativeWeightRegular, x, captionY+37, 0x777777)
	} else {
		l.label("No wallpaper received", 10, nativeWeightRegular, x, captionY+37, 0x777777)
	}
	if len(l.front.state.Apps) > 0 && captionY+54 < float32(l.height)-44 {
		line := fmt.Sprintf("%d apps shared by the phone", len(l.front.state.Apps))
		if l.front.state.AppLimit > 0 {
			line += fmt.Sprintf(" · limit %d", l.front.state.AppLimit)
		}
		l.label(clipRunes(line, 10, w), 10, nativeWeightRegular, x, captionY+54, 0x777777)
	}
}

func (l *nativeLauncher) phoneFrame() nativeRect {
	pad := float32(22)
	w := l.phone.w - pad*2
	top := float32(72)
	bottom := float32(l.height) - 112
	pH := w * 19.5 / 9
	if pH > bottom-top {
		pH = bottom - top
		w = pH * 9 / 19.5
	}
	return nativeRect{l.phone.x + (l.phone.w-w)/2, top + (bottom-top-pH)/2, w, pH}
}

func (l *nativeLauncher) drawFooter() {
	x := l.mainX + 26
	y := float32(l.height) - 26
	key := func(k, label string) {
		kw := l.textWidth(k, 9, nativeWeightMedium) + 12
		rect := nativeRect{x, y, kw, 17}
		l.round(rect, 5, 0x141414)
		l.labelCentered(k, 9, nativeWeightMedium, rect, colMuted)
		x += kw + 6
		lw := l.textWidth(label, 10, nativeWeightRegular)
		l.label(label, 10, nativeWeightRegular, x, y+3, colFaint)
		x += lw + 16
	}
	if l.searchFocus {
		key("type", "filter")
		key("↑↓", "move")
		key("Enter", "open")
		key("Esc", "clear")
	} else {
		key("↑↓", "move")
		key("Enter", "open")
		key("/", "search")
		key("D", "desktop")
		key("M", "mute")
	}

	right := l.mainRight - 26
	if l.front.errorText != "" {
		l.labelRight(clipRunes(l.front.errorText, 10, 430), 10, nativeWeightRegular, right, y+3, colNegative)
	}
}

func (l *nativeLauncher) drawToast() {
	if l.toast == "" || time.Now().After(l.toastEnd) {
		return
	}
	text := clipRunes(l.toast, 11, 520)
	tw := l.textWidth(text, 11, nativeWeightMedium)
	width := tw + 28
	x := l.mainX + (l.mainRight-l.mainX-width)/2
	y := float32(l.height) - l.footerH - 40
	rect := nativeRect{x, y, width, 28}
	l.round(rect, 999, 0x1c1c1c)
	color := uint32(colText)
	if l.toastBad {
		color = 0xffffff
		l.round(nativeRect{x + 11, y + 11, 6, 6}, 3, colNegative)
		l.label(text, 11, nativeWeightMedium, x+24, y+7, color)
		return
	}
	l.round(nativeRect{x + 11, y + 11, 6, 6}, 3, colAccent)
	l.label(text, 11, nativeWeightMedium, x+24, y+7, color)
}

func (l *nativeLauncher) mouseMove(x, y float32) {
	l.mouseX, l.mouseY = x, y
	l.hover = l.hitAt(x, y)
	pointer := false
	switch {
	case l.hover == "search":
		pointer = true
	case l.hover != "":
		pointer = true
	}
	enabled := C.int(0)
	if pointer {
		enabled = 1
	}
	C.nui_cursor(l.nativeCanvas.win, enabled)
}

func (l *nativeLauncher) activateAt(x, y float32) {
	id := l.hitAt(x, y)
	switch {
	case id == "tray":
		l.front.sendToTray()
	case id == "search":
		l.setSearchFocus(true)
		l.caret = time.Now()
	case id == "desktop":
		l.setSearchFocus(false)
		l.launch("desktop", "", "Mobile desktop")
	case id == "size720":
		l.setSearchFocus(false)
		l.sizeLarge = false
	case id == "size1080":
		l.setSearchFocus(false)
		l.sizeLarge = true
	case id == "sound":
		l.setSearchFocus(false)
		l.toggleMute()
	case strings.HasPrefix(id, "session:"):
		l.setSearchFocus(false)
		l.openSession(strings.TrimPrefix(id, "session:"))
	case strings.HasPrefix(id, "app:"):
		hit, _ := l.hit(id)
		l.setSearchFocus(false)
		if hit.app >= 0 && hit.app < len(l.apps) {
			l.selected = hit.app
			l.launchApp(hit.app)
		}
	default:
		l.setSearchFocus(false)
	}
}

func (l *nativeLauncher) mouseButton(x, y float32, button int, down bool) {
	l.mouseX, l.mouseY = x, y
	if button != 1 || !down {
		return
	}
	l.hover = l.hitAt(x, y)
	l.activateAt(x, y)
}

func (l *nativeLauncher) wheel(x, y, delta float32) {
	if !l.grid.contains(x, y) {
		return
	}
	l.scroll = max(0, min(l.scroll-delta*l.rowH*2, l.maxScroll))
	l.hover = l.hitAt(x, y)
}

func (l *nativeLauncher) touch(x, y float32, phase int) {
	if phase != 1 {
		return
	}
	l.mouseX, l.mouseY = x, y
	l.hover = l.hitAt(x, y)
	l.activateAt(x, y)
}

func (l *nativeLauncher) moveSelection(delta int) {
	if len(l.apps) == 0 {
		return
	}
	next := l.selected + delta
	if next < 0 {
		next = 0
	}
	if next >= len(l.apps) {
		next = len(l.apps) - 1
	}
	l.selected = next
	l.ensureVisible()
}

func (l *nativeLauncher) page(delta int) {
	if len(l.apps) == 0 || l.cols < 1 {
		return
	}
	rows := max(1, int(l.grid.h/l.rowH)-1)
	l.moveSelection(delta * l.cols * rows)
}

func (l *nativeLauncher) toggleSize() {
	l.sizeLarge = !l.sizeLarge
}

func (l *nativeLauncher) key(e C.native_event) {
	if e.down == 0 {
		return
	}
	scan := int(e.scan)
	mod := int(e.mods)
	ctrl := mod&int(C.SDL_KMOD_CTRL) != 0
	gui := mod&int(C.SDL_KMOD_GUI) != 0
	if (ctrl || gui) && scan == int(C.SDL_SCANCODE_Q) {
		l.front.closing = true
		return
	}
	if (ctrl || gui) && scan == int(C.SDL_SCANCODE_V) {
		l.pasteClipboard()
		return
	}
	if l.searchFocus {
		switch scan {
		case int(C.SDL_SCANCODE_ESCAPE):
			if l.search != "" {
				l.search = ""
				l.filter()
			} else {
				l.setSearchFocus(false)
			}
		case int(C.SDL_SCANCODE_RETURN), int(C.SDL_SCANCODE_KP_ENTER):
			l.launchApp(l.selected)
		case int(C.SDL_SCANCODE_BACKSPACE):
			if l.search != "" {
				_, size := utf8.DecodeLastRuneInString(l.search)
				l.search = l.search[:len(l.search)-size]
				l.filter()
			}
		case int(C.SDL_SCANCODE_A), int(C.SDL_SCANCODE_U):
			if ctrl || gui {
				l.search = ""
				l.filter()
			}
		case int(C.SDL_SCANCODE_TAB):
			l.setSearchFocus(false)
		case int(C.SDL_SCANCODE_DOWN):
			l.moveSelection(l.cols)
		case int(C.SDL_SCANCODE_UP):
			l.moveSelection(-l.cols)
		case int(C.SDL_SCANCODE_LEFT):
			l.moveSelection(-1)
		case int(C.SDL_SCANCODE_RIGHT):
			l.moveSelection(1)
		case int(C.SDL_SCANCODE_PAGEDOWN):
			l.page(1)
		case int(C.SDL_SCANCODE_PAGEUP):
			l.page(-1)
		case int(C.SDL_SCANCODE_HOME):
			l.selected = 0
			l.ensureVisible()
		case int(C.SDL_SCANCODE_END):
			l.selected = len(l.apps) - 1
			l.ensureVisible()
		}
		return
	}
	switch scan {
	case int(C.SDL_SCANCODE_SLASH):
		l.setSearchFocus(true)
		l.caret = time.Now()
	case int(C.SDL_SCANCODE_D):
		l.launch("desktop", "", "Mobile desktop")
	case int(C.SDL_SCANCODE_M):
		l.toggleMute()
	case int(C.SDL_SCANCODE_T):
		l.toggleSize()
	case int(C.SDL_SCANCODE_TAB):
		l.setSearchFocus(true)
	case int(C.SDL_SCANCODE_ESCAPE):
		l.front.setError("")
	case int(C.SDL_SCANCODE_RETURN), int(C.SDL_SCANCODE_KP_ENTER), int(C.SDL_SCANCODE_SPACE):
		l.launchApp(l.selected)
	case int(C.SDL_SCANCODE_DOWN):
		l.moveSelection(l.cols)
	case int(C.SDL_SCANCODE_UP):
		l.moveSelection(-l.cols)
	case int(C.SDL_SCANCODE_LEFT):
		l.moveSelection(-1)
	case int(C.SDL_SCANCODE_RIGHT):
		l.moveSelection(1)
	case int(C.SDL_SCANCODE_PAGEDOWN):
		l.page(1)
	case int(C.SDL_SCANCODE_PAGEUP):
		l.page(-1)
	case int(C.SDL_SCANCODE_HOME):
		l.selected = 0
		l.ensureVisible()
	case int(C.SDL_SCANCODE_END):
		l.selected = len(l.apps) - 1
		l.ensureVisible()
	}
}

func (l *nativeLauncher) pasteClipboard() {
	text := C.SDL_GetClipboardText()
	if text == nil {
		return
	}
	value := C.GoString(text)
	C.SDL_free(unsafe.Pointer(text))
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if remaining := max(0, 256-len(l.search)); len(value) > remaining {
		value = value[:remaining]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	l.search += value
	l.filter()
	l.setSearchFocus(true)
}

func (l *nativeLauncher) textInput(value string) {
	if !l.searchFocus {
		return
	}
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	if value == "" {
		return
	}
	if len(l.search)+len(value) > 256 {
		return
	}
	l.search += value
	l.filter()
}

type nativeTouchKey struct{ device, finger int64 }
type nativeTouch struct{ id, x, y int }

type nativeStream struct {
	nativeCanvas
	front *frontend

	session    viewerSession
	fullscreen bool
	focused    bool
	textOn     bool
	closed     bool

	headerH  float32
	header   nativeRect
	content  nativeRect
	video    nativeRect
	closeBtn nativeRect
	fullBtn  nativeRect
	segments [3]nativeRect
	segBox   nativeRect
	hover    string
	mouseX   float32
	mouseY   float32

	angle      int
	outW, outH int
	firstFrame bool
	lastDraw   string

	keys    map[int]int
	buttons map[int]bool
	touches map[nativeTouchKey]nativeTouch
}

func (f *frontend) frameDimensions(s viewerSession) (int, int) {
	if s.MediaID != "" {
		if w, h, ok := f.media.Dimensions(s.MediaID); ok && w > 0 && h > 0 {
			return w, h
		}
	}
	if s.Width > 0 && s.Height > 0 {
		return s.Width, s.Height
	}
	return 0, 0
}

// Rotation metadata describes the natural encoder raster's presentation.
// Android rotation 1 is counterclockwise on the receiver (270° in SDL).
// Manual orientation controls resize the phone display; they never add a
// second local rotation to the resulting picture.
func streamShape(s viewerSession, frameW, frameH int) (int, int, int) {
	w, h := frameW, frameH
	if w <= 0 || h <= 0 {
		w, h = s.Width, s.Height
	}
	if w <= 0 || h <= 0 {
		w, h = 1280, 720
	}
	switch s.Rotation {
	case 1:
		return h, w, 270
	case 2:
		return w, h, 180
	case 3:
		return h, w, 90
	default:
		return w, h, 0
	}
}

// rotatePoint maps an output-space point back into the source frame. It is the
// inverse of the clockwise rotation performed by native_video_draw_rotated.
func rotatePoint(u, v float64, angle int) (float64, float64) {
	switch angle {
	case 90:
		return v, 1 - u
	case 180:
		return 1 - u, 1 - v
	case 270:
		return 1 - v, u
	default:
		return u, v
	}
}

func (s *nativeStream) ready() bool {
	return s.front.state.Connected && s.session.State == "connected" && s.session.DisplayID >= 0
}

func (s *nativeStream) layout() {
	w, h := float32(s.width), float32(s.height)
	if w < 2 || h < 2 {
		return
	}
	// Keep controls reachable by pointer and touch in fullscreen too.
	s.headerH = 46
	s.header = nativeRect{0, 0, w, s.headerH}
	s.content = nativeRect{0, s.headerH, w, max(float32(0), h-s.headerH)}
	fw, fh := s.front.frameDimensions(s.session)
	ow, oh, angle := streamShape(s.session, fw, fh)
	if s.outW > 0 && (s.angle != angle || s.outW*oh != ow*s.outH) {
		s.release()
		s.resizeForOrientation()
	}
	s.outW, s.outH, s.angle = ow, oh, angle
	if s.outW <= 0 || s.outH <= 0 {
		s.outW, s.outH = 1280, 720
	}
	scale := min(s.content.w/float32(s.outW), s.content.h/float32(s.outH))
	vw, vh := float32(s.outW)*scale, float32(s.outH)*scale
	s.video = nativeRect{s.content.x + (s.content.w-vw)/2, s.content.y + (s.content.h-vh)/2, vw, vh}

	// Reserve controls first; narrow portrait windows omit the title.
	labels := []string{"Auto", "Landscape", "Portrait"}
	if w < 700 {
		labels = []string{"Auto", "Land", "Port"}
	}
	total := float32(6)
	for _, label := range labels {
		total += max(float32(46), s.textWidth(label, 10, nativeWeightMedium)+18)
	}
	s.closeBtn = nativeRect{w - 12 - 30, 8, 30, 30}
	s.fullBtn = nativeRect{s.closeBtn.x - 86, 8, 80, 30}
	boxW := total
	boxX := s.fullBtn.x - 8 - boxW
	s.segBox = nativeRect{boxX, 9, boxW, 28}
	x := boxX + 3
	for i, label := range labels {
		width := max(float32(46), s.textWidth(label, 10, nativeWeightMedium)+18)
		s.segments[i] = nativeRect{x, 12, width, 22}
		x += width
	}
}

func (s *nativeStream) headerHit(x, y float32) string {
	if !s.header.contains(x, y) {
		return ""
	}
	if s.closeBtn.contains(x, y) {
		return "close"
	}
	if s.fullBtn.contains(x, y) {
		return "fullscreen"
	}
	for i, rect := range s.segments {
		// The visual control is a 28px-tall pill; give it the full pill height.
		if x >= rect.x && x < rect.x+rect.w && y >= 9 && y < 37 {
			return []string{"auto", "landscape", "portrait"}[i]
		}
	}
	return ""
}

func (s *nativeStream) draw() {
	s.begin()
	w, h := float32(s.width), float32(s.height)
	s.fill(nativeRect{0, 0, w, h}, colGround)
	s.drawHeader()

	message, detail, color := s.statusMessage()
	drawError := ""
	if s.ready() && s.session.MediaID != "" {
		err := s.front.media.DrawRotated(s.session.MediaID, unsafe.Pointer(C.nui_window_renderer(s.nativeCanvas.win)),
			s.video.x, s.video.y, s.video.w, s.video.h, s.angle)
		switch err {
		case nil:
			s.firstFrame = true
		case errNativeWaitingFrame:
			if !s.firstFrame {
				message, detail, color = "Waiting for the first frame", "The video channel is open.", colMuted
			}
		case errNativeWaitingTransport, errNativeWaitingTrack:
			if !s.firstFrame {
				message, detail, color = "Opening video", "The phone is starting the encoder.", colMuted
			}
		default:
			drawError = err.Error()
		}
	}
	if s.session.State == "failed" {
		message = s.session.Error
		detail = "Close this window to release the session."
		color = colNegative
	}

	if message != "" && (!s.firstFrame || s.session.State == "failed") {
		s.clip(s.content)
		line := clipRunes(message, 15, max(float32(120), s.content.w-80))
		lw := s.textWidth(line, 15, nativeWeightMedium)
		lx := s.content.x + (s.content.w-lw)/2
		ly := s.content.y + s.content.h/2 - 24
		s.label(line, 15, nativeWeightMedium, lx, ly, color)
		if detail != "" {
			dl := clipRunes(detail, 11, max(float32(120), s.content.w-80))
			dw := s.textWidth(dl, 11, nativeWeightRegular)
			s.label(dl, 11, nativeWeightRegular, s.content.x+(s.content.w-dw)/2, ly+26, colDim)
		}
		if s.session.State != "failed" && drawError == "" {
			s.loading(s.content.x+(s.content.w-min(float32(220), s.content.w*0.5))/2, ly+56, min(float32(220), s.content.w*0.5))
		}
		s.unclip()
	} else if s.firstFrame {
		if s.session.State == "paused" {
			pill := "Paused on the phone"
			pw := s.textWidth(pill, 10, nativeWeightMedium) + 30
			rect := nativeRect{s.content.x + (s.content.w-pw)/2, s.content.y + s.content.h - 44, pw, 28}
			s.roundAlpha(rect, 999, 0x000000, 200)
			s.round(nativeRect{rect.x + 12, rect.y + 11, 6, 6}, 3, colWarning)
			s.label(pill, 10, nativeWeightMedium, rect.x+24, rect.y+7, colText)
		}
	}
	if drawError != "" && s.session.State != "failed" {
		s.clip(s.content)
		line := clipRunes("Video: "+drawError, 10, s.content.w-40)
		s.label(line, 10, nativeWeightRegular, s.content.x+20, s.content.y+s.content.h-26, colNegative)
		s.unclip()
	}
	if s.session.OrientationPending != "" || s.session.OrientationError != "" {
		text, color := "Requesting "+s.session.OrientationPending+" on phone…", uint32(colMuted)
		if s.session.OrientationError != "" {
			text, color = "Orientation: "+s.session.OrientationError, colNegative
		}
		s.clip(s.content)
		s.label(clipRunes(text, 11, s.content.w-32), 11, nativeWeightRegular,
			s.content.x+16, s.content.y+s.content.h-48, color)
		s.unclip()
	}
	s.present()
}

func (s *nativeStream) statusMessage() (string, string, uint32) {
	switch s.session.State {
	case "requesting":
		return "Waiting for " + s.session.Name, "Approve the window on your phone if asked.", colMuted
	case "negotiating":
		return "Connecting video", "The phone is preparing the stream.", colMuted
	case "paused":
		return "Paused on the phone", "Bring the app back to the foreground to resume.", colMuted
	case "failed":
		return s.session.Error, "Close this window to release the session.", colNegative
	case "connected":
		return "Waiting for video", "The phone display is connected.", colMuted
	default:
		return "Preparing the window", "", colMuted
	}
}

func (s *nativeStream) loading(x, y, width float32) {
	phase := float32(time.Now().UnixMilli()%1400) / 1400
	s.round(nativeRect{x, y, width, 2}, 1, colRule)
	thumb := float32(52)
	pos := (width + thumb) * phase
	if pos > width {
		pos -= width + thumb
	}
	start := max(float32(0), pos-thumb)
	length := min(thumb, width-start)
	s.round(nativeRect{x + start, y, length, 2}, 1, 0xb4b4b4)
}

func (s *nativeStream) drawHeader() {
	w := float32(s.width)
	s.fill(s.header, 0x080808)
	s.fill(nativeRect{0, s.headerH - 1, w, 1}, colRule)
	pkg := ""
	if s.front.launcher != nil {
		pkg = s.front.launcher.appForName(s.session.Name)
	}
	iconRect := nativeRect{14, 11, 24, 24}
	s.appIcon(viewerApp{Name: s.session.Name, Package: pkg}, iconRect)

	textEnd := s.segBox.x - 10
	if textEnd < 60 {
		textEnd = 60
	}
	if s.segBox.x >= 130 {
		name := clipRunes(s.session.Name, 12, textEnd-46)
		s.label(name, 12, nativeWeightSemibold, 46, 6, colText)
		state := s.session.State
		color := uint32(colFaint)
		switch s.session.State {
		case "connected":
			state = "Live"
			color = colFaint
		case "requesting":
			state = "Waiting for phone"
		case "negotiating":
			state = "Connecting"
		case "paused":
			state = "Paused"
			color = colWarning
		case "failed":
			state = "Failed"
			color = colNegative
		}
		sub := fmt.Sprintf("%d × %d · %s", s.session.Width, s.session.Height, state)
		if s.fullscreen {
			sub += " · fullscreen"
		}
		s.label(clipRunes(sub, 10, textEnd-46), 10, nativeWeightRegular, 46, 24, color)
	}

	s.round(s.segBox, 999, colSurface)
	for i, rect := range s.segments {
		active := orientationModes[i] == s.session.Orientation || i == orientAuto && s.session.Orientation == ""
		if active {
			s.round(rect, 999, colText)
		} else if s.hover == []string{"auto", "landscape", "portrait"}[i] {
			s.round(rect, 999, 0x1c1c1c)
		}
		label := []string{"Auto", "Landscape", "Portrait"}[i]
		if w < 700 {
			label = []string{"Auto", "Land", "Port"}[i]
		}
		textColor := uint32(colMuted)
		if active {
			textColor = 0x000000
		}
		s.labelCentered(label, 10, nativeWeightMedium, rect, textColor)
	}
	fullColor := uint32(colMuted)
	if s.hover == "fullscreen" {
		s.round(s.fullBtn, 6, colRaised)
		fullColor = colText
	}
	fullLabel := "Fullscreen"
	if s.fullscreen {
		fullLabel = "Exit full"
	}
	s.labelCentered(fullLabel, 10, nativeWeightMedium, s.fullBtn, fullColor)
	closeColor := uint32(colMuted)
	if s.hover == "close" {
		s.round(s.closeBtn, 15, 0x1c1c1c)
		closeColor = colText
	}
	s.labelCentered("×", 17, nativeWeightRegular, nativeRect{s.closeBtn.x, s.closeBtn.y - 1, s.closeBtn.w, s.closeBtn.h}, closeColor)
}

func (s *nativeStream) setFullscreen(enabled bool) {
	if s.fullscreen == enabled {
		return
	}
	on := C.int(0)
	if enabled {
		on = 1
	}
	if C.nui_window_fullscreen(s.nativeCanvas.win, on) == 0 {
		s.front.setError("Fullscreen: " + C.GoString(C.SDL_GetError()))
		return
	}
	s.fullscreen = enabled
	s.release()
	s.refresh()
	s.layout()
}

func (s *nativeStream) setOrientation(mode int) {
	if mode < orientAuto || mode > orientPortrait {
		return
	}
	if _, err := s.front.hub.command(viewerCommand{
		Op: "orientation", SessionID: s.session.ID, Orientation: orientationModes[mode],
	}); err != nil {
		s.front.setError("Orientation: " + err.Error())
	}
}

func (s *nativeStream) resizeForOrientation() {
	if s.fullscreen {
		return
	}
	fw, fh := s.front.frameDimensions(s.session)
	cw, ch, _ := streamShape(s.session, fw, fh)
	ww, wh := s.front.streamWindowSize(cw, ch)
	var x, y, ow, oh C.int
	C.nui_window_get_position(s.nativeCanvas.win, &x, &y)
	C.nui_window_get_size(s.nativeCanvas.win, &ow, &oh, nil)
	C.nui_window_set_size(s.nativeCanvas.win, C.int(ww), C.int(wh))
	C.nui_window_set_position(s.nativeCanvas.win, x+(ow-C.int(ww))/2, y+(oh-C.int(wh))/2)
}

func (s *nativeStream) syncSession(next viewerSession) {
	old := s.session
	s.session = next
	if old.Name != next.Name {
		setNativeTitle(s.nativeCanvas.win, next.Name+" — rfproto")
	}
	if old.MediaID != next.MediaID || old.DisplayID != next.DisplayID || next.State != "connected" {
		s.release()
		s.firstFrame = false
	}
	if old.State != next.State {
		s.lastDraw = ""
	}
	// Signaling and frames can arrive in either order. Layout resizes only
	// when the displayed frame shape changes, rather than using a stale frame.
	if old.Width != next.Width || old.Height != next.Height || old.Rotation != next.Rotation {
		s.release()
	}
}

func (s *nativeStream) focusedEvent(on bool) {
	s.focused = on
	if !on {
		s.textOn = false
		C.nui_text_input(s.nativeCanvas.win, 0, 0, 0, 0, 0)
		s.release()
		return
	}
	if !s.textOn {
		C.nui_text_input(s.nativeCanvas.win, 1, 0, 0, 0, 0)
		s.textOn = true
	}
}

func (s *nativeStream) send(in viewerInput) {
	if !s.ready() {
		return
	}
	_, _ = s.front.hub.command(viewerCommand{Op: "input", SessionID: s.session.ID, Input: &in})
}

func (s *nativeStream) release() {
	if len(s.keys) == 0 && len(s.buttons) == 0 && len(s.touches) == 0 {
		return
	}
	if s.session.MediaID != "" {
		_, _ = s.front.hub.command(viewerCommand{Op: "input", SessionID: s.session.ID, Input: &viewerInput{Type: "release"}})
	}
	clear(s.keys)
	clear(s.buttons)
	clear(s.touches)
	C.nui_mouse_capture(s.front.ui, 0)
}

func (s *nativeStream) close(stop bool) {
	if s.closed {
		return
	}
	s.closed = true
	s.release()
	if stop && s.session.ID != "" {
		_, _ = s.front.hub.command(viewerCommand{Op: "stop", SessionID: s.session.ID})
	}
	delete(s.front.streams, s.session.ID)
	delete(s.front.streamByWindow, s.nativeCanvas.win)
	s.front.closingSessions[s.session.ID] = time.Now()
	s.nativeCanvas.close()
	C.nui_window_free(s.nativeCanvas.win)
}

func (s *nativeStream) point(x, y float32, clampPoint bool) (int, int, bool) {
	if !s.ready() || s.video.w <= 0 || s.video.h <= 0 {
		return 0, 0, false
	}
	if !clampPoint && !s.video.contains(x, y) {
		return 0, 0, false
	}
	u := float64((x - s.video.x) / s.video.w)
	v := float64((y - s.video.y) / s.video.h)
	u = math.Max(0, math.Min(1, u))
	v = math.Max(0, math.Min(1, v))
	u, v = rotatePoint(u, v, s.angle)
	px := int(u * float64(s.session.Width))
	py := int(v * float64(s.session.Height))
	return max(0, min(s.session.Width-1, px)), max(0, min(s.session.Height-1, py)), true
}

func (s *nativeStream) mouseMove(x, y float32) {
	s.mouseX, s.mouseY = x, y
	hover := s.headerHit(x, y)
	if hover != s.hover {
		s.hover = hover
	}
	pointer := C.int(0)
	if hover != "" {
		pointer = 1
	}
	C.nui_cursor(s.nativeCanvas.win, pointer)
	if s.video.contains(x, y) && s.ready() {
		if px, py, ok := s.point(x, y, false); ok {
			s.send(viewerInput{Type: "move", X: px, Y: py})
		}
	}
}

func (s *nativeStream) mouseButton(x, y float32, button int, down bool) {
	if down {
		if header := s.headerHit(x, y); header != "" {
			switch header {
			case "close":
				s.close(true)
			case "fullscreen":
				s.setFullscreen(!s.fullscreen)
			case "auto":
				s.setOrientation(orientAuto)
			case "landscape":
				s.setOrientation(orientLandscape)
			case "portrait":
				s.setOrientation(orientPortrait)
			}
			return
		}
	}
	if button < 1 || button > 3 {
		return
	}
	if down {
		px, py, ok := s.point(x, y, false)
		if !ok {
			return
		}
		s.buttons[button] = true
		C.nui_mouse_capture(s.front.ui, 1)
		s.send(viewerInput{Type: "button", Key: button, Down: true, X: px, Y: py})
		return
	}
	if !s.buttons[button] {
		return
	}
	px, py, ok := s.point(x, y, true)
	if ok {
		s.send(viewerInput{Type: "button", Key: button, X: px, Y: py})
	}
	delete(s.buttons, button)
	if len(s.buttons) == 0 {
		C.nui_mouse_capture(s.front.ui, 0)
	}
}

func (s *nativeStream) wheel(x, y, delta float32) {
	if delta == 0 || !s.video.contains(x, y) {
		return
	}
	px, py, ok := s.point(x, y, false)
	if !ok {
		return
	}
	s.send(viewerInput{Type: "wheel", X: px, Y: py, Down: delta < 0})
}

func (s *nativeStream) touch(x, y float32, phase int, finger, device int64) {
	key := nativeTouchKey{device, finger}
	held, exists := s.touches[key]
	if phase == 1 {
		if header := s.headerHit(x, y); header != "" {
			switch header {
			case "close":
				s.close(true)
			case "fullscreen":
				s.setFullscreen(!s.fullscreen)
			case "auto":
				s.setOrientation(orientAuto)
			case "landscape":
				s.setOrientation(orientLandscape)
			case "portrait":
				s.setOrientation(orientPortrait)
			}
			return
		}
		px, py, ok := s.point(x, y, false)
		if !ok {
			return
		}
		held = nativeTouch{id: int(finger % 16), x: px, y: py}
		exists = true
	}
	if !exists {
		return
	}
	px, py, ok := s.point(x, y, true)
	if !ok {
		return
	}
	held.x, held.y = px, py
	if s.session.Touch {
		pressure := 0.5
		if phase == 0 || phase == 3 {
			pressure = 0
		}
		s.send(viewerInput{Type: "touch", Event: phase, PointerID: held.id, X: px, Y: py, Pressure: pressure, MajorAxisSize: 1})
	} else if phase == 2 {
		s.send(viewerInput{Type: "move", X: px, Y: py})
	} else {
		s.send(viewerInput{Type: "button", Key: 1, Down: phase == 1, X: px, Y: py})
	}
	if phase == 0 || phase == 3 {
		delete(s.touches, key)
	} else {
		s.touches[key] = held
	}
}

func (s *nativeStream) key(e C.native_event) {
	scan := int(e.scan)
	mod := int(e.mods)
	ctrl := mod&int(C.SDL_KMOD_CTRL) != 0
	gui := mod&int(C.SDL_KMOD_GUI) != 0
	if e.down == 0 {
		if s.ready() {
			if in, ok := nativeKeyboardInput(scan, mod, int(e.key), false, false, s.keys); ok {
				s.send(in)
			}
		}
		return
	}
	if scan == int(C.SDL_SCANCODE_F11) {
		if e.repeat == 0 {
			s.setFullscreen(!s.fullscreen)
		}
		return
	}
	if (ctrl || gui) && scan == int(C.SDL_SCANCODE_W) {
		s.close(true)
		return
	}
	if scan == int(C.SDL_SCANCODE_ESCAPE) && s.fullscreen {
		s.setFullscreen(false)
		return
	}
	if (ctrl || gui) && scan == int(C.SDL_SCANCODE_V) || mod&int(C.SDL_KMOD_SHIFT) != 0 && scan == int(C.SDL_SCANCODE_INSERT) {
		text := C.SDL_GetClipboardText()
		if text != nil {
			value := C.GoString(text)
			C.SDL_free(unsafe.Pointer(text))
			if value != "" && len(value) <= 65536 {
				s.send(viewerInput{Type: "text", Text: value})
			}
		}
		return
	}
	if !s.ready() || s.session.MediaID == "" {
		return
	}
	if in, ok := nativeKeyboardInput(scan, mod, int(e.key), true, false, s.keys); ok {
		s.send(in)
	}
}

func (s *nativeStream) textInput(value string) {
	if value == "" {
		return
	}
	if len(value) <= 65536 {
		s.send(viewerInput{Type: "text", Text: value})
	}
}

// frontendRequest crosses from HTTP handlers to the SDL event thread.
type frontendRequest struct {
	op, session string
	mute        bool
	result      chan error
}

type frontend struct {
	ui     *C.native_ui
	hub    *viewerHub
	media  *nativeMedia
	assets *frontendObserver

	launcher       *nativeLauncher
	streams        map[string]*nativeStream
	streamByWindow map[*C.native_window]*nativeStream

	state           viewerState
	closingSessions map[string]time.Time
	pendingFocus    string
	closing         bool
	errorText       string
	errorUntil      time.Time
	requests        chan frontendRequest
	done            chan struct{}
	muted           atomic.Bool
	tray            *systemTray
	inTray          bool

	lastSnapshot      uint64
	lastSnapshotCheck time.Time
}

func (f *frontend) setError(text string) {
	f.errorText = text
	if text != "" {
		f.errorUntil = time.Now().Add(10 * time.Second)
		if f.launcher != nil {
			f.launcher.toastMessage(text, true)
		}
	}
}

func (f *frontend) applyState(state viewerState) {
	wasConnected := f.state.Connected
	f.state = state
	if f.launcher != nil {
		f.launcher.filter()
		if wasConnected != state.Connected {
			if state.Connected {
				f.launcher.toastMessage("Phone connected", false)
				f.errorText = ""
			} else {
				f.launcher.toastMessage("Phone disconnected", true)
			}
		}
	}
	if state.Audio.Error != "" {
		f.errorText = state.Audio.Error
		f.errorUntil = time.Now().Add(10 * time.Second)
	}
}

func (f *frontend) syncStreams() {
	if f.launcher == nil {
		return
	}
	current := make(map[string]viewerSession, len(f.state.Sessions))
	for _, s := range f.state.Sessions {
		current[s.ID] = s
		if st := f.streams[s.ID]; st != nil {
			st.syncSession(s)
			continue
		}
		if _, closing := f.closingSessions[s.ID]; closing {
			continue
		}
		f.createStream(s)
	}
	for id, st := range f.streams {
		if _, ok := current[id]; !ok {
			st.close(false)
		}
	}
	for id := range f.closingSessions {
		if _, ok := current[id]; !ok {
			delete(f.closingSessions, id)
		}
	}
	if f.pendingFocus != "" {
		if st := f.streams[f.pendingFocus]; st != nil {
			C.nui_window_raise(st.nativeCanvas.win)
			f.pendingFocus = ""
		}
	}
}

func (f *frontend) createStream(s viewerSession) {
	fw, fh := f.frameDimensions(s)
	cw, ch, _ := streamShape(s, fw, fh)
	ww, wh := f.streamWindowSize(cw, ch)
	title := s.Name + " — rfproto"
	cs := C.CString(title)
	win := C.nui_window_new(f.ui, cs, C.int(ww), C.int(wh), 360, 280)
	C.free(unsafe.Pointer(cs))
	if win == nil {
		f.setError("Open app window: " + C.GoString(C.SDL_GetError()))
		return
	}
	st := &nativeStream{front: f, session: s, keys: make(map[int]int),
		buttons: make(map[int]bool), touches: make(map[nativeTouchKey]nativeTouch)}
	st.nativeCanvas = *newNativeCanvas(win, f.assets)
	st.refresh()
	st.layout()
	pkg := ""
	if f.launcher != nil {
		pkg = f.launcher.appForName(s.Name)
	}
	pixels := f.appIconPixels(pkg, s.Name)
	C.nui_window_icon(win, unsafe.Pointer(&pixels[0]), 64, 64)
	f.streams[s.ID] = st
	f.streamByWindow[win] = st
	f.positionNewWindow(win, ww, wh)
	if f.inTray {
		C.nui_window_hide(win)
	}
}

func (f *frontend) streamWindowSize(cw, ch int) (int, int) {
	var dw, dh C.int
	C.nui_display_usable(f.ui, &dw, &dh)
	if cw <= 0 || ch <= 0 {
		cw, ch = 1280, 720
	}
	maxW := min(float32(1200), float32(dw)*0.72)
	maxH := min(float32(760), float32(dh)*0.82-46)
	if maxH < 220 {
		maxH = 220
	}
	scale := min(maxW/float32(cw), maxH/float32(ch))
	if scale > 1.3 {
		scale = 1.3
	}
	if scale <= 0 {
		scale = 1
	}
	w := max(360, int(float32(cw)*scale))
	h := max(300, int(float32(ch)*scale)+46)
	return w, h
}

func (f *frontend) positionNewWindow(win *C.native_window, w, h int) {
	var dw, dh C.int
	C.nui_display_usable(f.ui, &dw, &dh)
	cascade := (len(f.streams) % 5) * 26
	x := (int(dw)-w)/2 + cascade
	y := (int(dh)-h)/2 + cascade
	C.nui_window_set_position(win, C.int(x), C.int(y))
}

func (f *frontend) focusSession(id string) {
	if st := f.streams[id]; st != nil {
		f.restoreFromTray()
		C.nui_window_raise(st.nativeCanvas.win)
	}
}

func (f *frontend) setMuted(muted bool) {
	f.media.SetMuted(muted)
	f.muted.Store(muted)
	if f.launcher != nil {
		f.launcher.muted = muted
	}
}

func (f *frontend) request(ctx context.Context, req frontendRequest) error {
	req.result = make(chan error, 1)
	select {
	case f.requests <- req:
	case <-f.done:
		return fmt.Errorf("frontend stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-f.done:
		return fmt.Errorf("frontend stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *frontend) handleRequest(req frontendRequest) {
	if req.op == "mute" {
		if f.media == nil {
			req.result <- fmt.Errorf("media not running")
			return
		}
		f.setMuted(req.mute)
	} else {
		st := f.streams[req.session]
		if st == nil || st.closed {
			req.result <- errFrontendSession
			return
		}
		switch req.op {
		case "focus":
			f.focusSession(req.session)
		}
	}
	req.result <- nil
}

func (f *frontend) handleHub(event any) {
	message, ok := event.(map[string]any)
	if !ok {
		return
	}
	switch message["type"] {
	case "state":
		state, ok := message["state"].(viewerState)
		if !ok {
			return
		}
		f.applyState(state)
		f.syncStreams()
	case "focus":
		if id, ok := message["sessionId"].(string); ok {
			f.focusSession(id)
		}
	case "error":
		if text, ok := message["message"].(string); ok {
			f.setError(text)
		}
	}
}

func (f *frontend) sendToTray() {
	if f.tray == nil {
		var err error
		f.tray, err = newSystemTray(f.appIconPixels("", "rfproto"))
		if err != nil {
			f.setError(err.Error())
			return
		}
	} else if err := f.tray.register(); err != nil {
		f.setError(err.Error())
		return
	}
	f.inTray = true
	f.launcher.focusedEvent(false)
	C.nui_window_hide(f.launcher.win)
	for _, st := range f.streams {
		st.focusedEvent(false)
		C.nui_window_hide(st.win)
	}
}

func (f *frontend) restoreFromTray() {
	if !f.inTray {
		return
	}
	f.inTray = false
	for _, st := range f.streams {
		C.nui_window_raise(st.win)
	}
	C.nui_window_raise(f.launcher.win)
}

func (f *frontend) closeAll() {
	if f.tray != nil {
		f.tray.close()
	}
	for _, st := range f.streams {
		st.close(false)
	}
	if f.launcher != nil {
		f.launcher.nativeCanvas.close()
		C.nui_window_free(f.launcher.nativeCanvas.win)
		f.launcher = nil
	}
}

func (f *frontend) appIconPixels(pkg, name string) []byte {
	if pkg != "" && f.assets != nil {
		if data, err := os.ReadFile(f.assets.IconPath(pkg)); err == nil {
			if img, _, err := image.Decode(strings.NewReader(string(data))); err == nil {
				rgba := resizeIcon(img, 64)
				return append([]byte(nil), rgba.Pix...)
			}
		}
	}
	tint := appTint(name)
	out := make([]byte, 64*64*4)
	fill := func(x, y int, r, g, b, a uint8) {
		if x < 0 || y < 0 || x >= 64 || y >= 64 {
			return
		}
		index := (y*64 + x) * 4
		out[index], out[index+1], out[index+2], out[index+3] = r, g, b, a
	}
	tr := uint8(tint >> 16)
	tg := uint8(tint >> 8)
	tb := uint8(tint)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			dx := min(x, 63-x)
			dy := min(y, 63-y)
			if dx < 16 && dy < 16 {
				cx, cy := 16-dx, 16-dy
				if cx*cx+cy*cy > 16*16 {
					continue
				}
			}
			fill(x, y, tr, tg, tb, 255)
		}
	}
	letter := ' '
	for _, r := range strings.ToUpper(name) {
		if r >= 'A' && r <= 'Z' {
			letter = r
			break
		}
	}
	glyph, ok := glyph5x7[letter]
	if !ok {
		return out
	}
	scale := 6
	offsetX := (64 - 5*scale) / 2
	offsetY := (64 - 7*scale) / 2
	for gy, row := range glyph {
		for gx, cell := range row {
			if cell != '#' {
				continue
			}
			for y := 0; y < scale; y++ {
				for x := 0; x < scale; x++ {
					fill(offsetX+gx*scale+x, offsetY+gy*scale+y, 255, 255, 255, 255)
				}
			}
		}
	}
	return out
}

var glyph5x7 = map[rune][]string{
	'A': {" ### ", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"},
	'B': {"#### ", "#   #", "#   #", "#### ", "#   #", "#   #", "#### "},
	'C': {" ### ", "#   #", "#    ", "#    ", "#    ", "#   #", " ### "},
	'D': {"#### ", "#   #", "#   #", "#   #", "#   #", "#   #", "#### "},
	'E': {"#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#####"},
	'F': {"#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#    "},
	'G': {" ### ", "#   #", "#    ", "# ###", "#   #", "#   #", " ####"},
	'H': {"#   #", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"},
	'I': {"#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "#####"},
	'J': {"  ###", "   # ", "   # ", "   # ", "#  # ", "#  # ", " ##  "},
	'K': {"#   #", "#  # ", "# #  ", "##   ", "# #  ", "#  # ", "#   #"},
	'L': {"#    ", "#    ", "#    ", "#    ", "#    ", "#    ", "#####"},
	'M': {"#   #", "## ##", "# # #", "# # #", "#   #", "#   #", "#   #"},
	'N': {"#   #", "##  #", "# # #", "#  ##", "#   #", "#   #", "#   #"},
	'O': {" ### ", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "},
	'P': {"#### ", "#   #", "#   #", "#### ", "#    ", "#    ", "#    "},
	'Q': {" ### ", "#   #", "#   #", "#   #", "# # #", "#  # ", " ## #"},
	'R': {"#### ", "#   #", "#   #", "#### ", "# #  ", "#  # ", "#   #"},
	'S': {" ####", "#    ", "#    ", " ### ", "    #", "    #", "#### "},
	'T': {"#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  "},
	'U': {"#   #", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "},
	'V': {"#   #", "#   #", "#   #", "#   #", "#   #", " # # ", "  #  "},
	'W': {"#   #", "#   #", "#   #", "# # #", "# # #", "## ##", "#   #"},
	'X': {"#   #", "#   #", " # # ", "  #  ", " # # ", "#   #", "#   #"},
	'Y': {"#   #", "#   #", " # # ", "  #  ", "  #  ", "  #  ", "  #  "},
	'Z': {"#####", "    #", "   # ", "  #  ", " #   ", "#    ", "#####"},
}

func runNativeViewer(h *viewerHub, assets *frontendObserver, stop <-chan os.Signal) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if len(nativeUIFont) == 0 {
		return fmt.Errorf("embedded Inter font is empty")
	}
	ui := C.nui_open(unsafe.Pointer(&nativeUIFont[0]), C.size_t(len(nativeUIFont)))
	if ui == nil {
		return fmt.Errorf("native UI: %s", C.GoString(C.SDL_GetError()))
	}
	defer func() {
		setControlFrontend(nil)
		C.nui_close(ui)
	}()
	log.Printf("[ui] SDL multi-window frontend ready (video=%s)", C.GoString(C.SDL_GetCurrentVideoDriver()))

	media := newNativeMedia(h)
	defer media.Close()
	events, unsubscribe, err := h.subscribe()
	if err != nil {
		return err
	}
	defer unsubscribe()

	var dw, dh C.int
	C.nui_display_usable(ui, &dw, &dh)
	winW := min(1360, int(dw)-80)
	winH := min(880, int(dh)-70)
	winW = max(900, winW)
	winH = max(600, winH)
	title := C.CString("rfproto")
	win := C.nui_window_new(ui, title, C.int(winW), C.int(winH), 900, 600)
	C.free(unsafe.Pointer(title))
	if win == nil {
		return fmt.Errorf("launcher window: %s", C.GoString(C.SDL_GetError()))
	}
	f := &frontend{ui: ui, hub: h, media: media, assets: assets,
		streams: make(map[string]*nativeStream), streamByWindow: make(map[*C.native_window]*nativeStream),
		closingSessions: make(map[string]time.Time), requests: make(chan frontendRequest), done: make(chan struct{})}
	defer close(f.done)
	setControlFrontend(f)
	l := &nativeLauncher{front: f, selected: -1, caret: time.Now()}
	l.nativeCanvas = *newNativeCanvas(win, assets)
	l.refresh()
	l.layout()
	f.launcher = l
	if assets != nil {
		l.snapshot = assets.Snapshot()
		l.nativeCanvas.syncWallpaper(l.snapshot)
		l.nativeCanvas.syncPairingQR(l.snapshot)
		f.lastSnapshot = l.snapshot.Version
	}
	iconPixels := f.appIconPixels("", "rfproto")
	C.nui_window_icon(win, unsafe.Pointer(&iconPixels[0]), 64, 64)
	var px, py C.int
	C.nui_window_get_position(win, &px, &py)
	C.nui_window_set_position(win, C.int(max(0, (int(dw)-winW)/2)), C.int(max(0, (int(dh)-winH)/2)))

	f.applyState(h.state())
	f.syncStreams()
	defer f.closeAll()

	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()
	for {
		if f.tray != nil && f.tray.wantsShow() {
			f.restoreFromTray()
		}
		if f.closing {
			return nil
		}
		if assets != nil && time.Since(f.lastSnapshotCheck) > 250*time.Millisecond {
			f.lastSnapshotCheck = time.Now()
			snapshot := assets.Snapshot()
			if snapshot.Version != f.lastSnapshot {
				f.lastSnapshot = snapshot.Version
				l.snapshot = snapshot
				l.nativeCanvas.syncWallpaper(snapshot)
				l.nativeCanvas.syncPairingQR(snapshot)
			}
		}
		if f.errorText != "" && time.Now().After(f.errorUntil) {
			f.errorText = ""
		}

		select {
		case req := <-f.requests:
			f.handleRequest(req)
		default:
		}
		for n := 0; n < 512; n++ {
			select {
			case event, ok := <-events:
				if !ok {
					f.closing = true
					break
				}
				media.Handle(event)
				f.handleHub(event)
			default:
				n = 512
			}
		}

		l.refresh()
		l.layout()
		l.updateTextInput()
		for _, st := range f.streams {
			if st.closed {
				continue
			}
			st.refresh()
			st.layout()
		}

		var event C.native_event
		for n := 0; n < 512; n++ {
			if C.nui_poll(ui, &event) == 0 {
				break
			}
			if event.kind == C.NUI_QUIT {
				f.closing = true
				continue
			}
			window := event.window
			launcherWindow := window == l.nativeCanvas.win
			stream := f.streamByWindow[window]
			if !launcherWindow && stream == nil {
				continue
			}
			switch event.kind {
			case C.NUI_WINDOW_CLOSE:
				if launcherWindow {
					f.closing = true
				} else {
					stream.close(true)
				}
			case C.NUI_FOCUS:
				if launcherWindow {
					l.focusedEvent(true)
				} else {
					stream.focusedEvent(true)
				}
			case C.NUI_BLUR:
				if launcherWindow {
					l.focusedEvent(false)
				} else {
					stream.focusedEvent(false)
				}
			case C.NUI_KEY:
				if launcherWindow {
					l.key(event)
				} else {
					stream.key(event)
				}
			case C.NUI_TEXT:
				text := C.GoString(event.text)
				if launcherWindow {
					l.textInput(text)
				} else {
					stream.textInput(text)
				}
			case C.NUI_MOVE:
				if launcherWindow {
					l.mouseMove(float32(event.x), float32(event.y))
				} else {
					stream.mouseMove(float32(event.x), float32(event.y))
				}
			case C.NUI_BUTTON:
				if launcherWindow {
					l.mouseButton(float32(event.x), float32(event.y), int(event.button), event.down != 0)
				} else {
					stream.mouseButton(float32(event.x), float32(event.y), int(event.button), event.down != 0)
				}
			case C.NUI_WHEEL:
				if launcherWindow {
					l.wheel(float32(event.x), float32(event.y), float32(event.wheel))
				} else {
					stream.wheel(float32(event.x), float32(event.y), float32(event.wheel))
				}
			case C.NUI_TOUCH:
				if launcherWindow {
					l.touch(float32(event.x)*float32(l.width), float32(event.y)*float32(l.height), int(event.phase))
				} else {
					stream.touch(float32(event.x)*float32(stream.width), float32(event.y)*float32(stream.height), int(event.phase), int64(event.finger), int64(event.device))
				}
			}
		}

		l.refresh()
		l.layout()
		l.updateTextInput()
		if !f.inTray {
			l.draw()
		}
		for _, st := range f.streams {
			if st.closed {
				continue
			}
			st.refresh()
			st.layout()
			if !f.inTray {
				st.draw()
			}
		}

		select {
		case <-stop:
			f.closing = true
		case <-tick.C:
		}
	}
}

// setNativeTitle copies Go text into a temporary C string; SDL keeps its own.
func setNativeTitle(win *C.native_window, title string) {
	cs := C.CString(title)
	C.nui_window_set_title(win, cs)
	C.free(unsafe.Pointer(cs))
}

func nativeMeta(mod int) int {
	state := 0
	if mod&int(C.SDL_KMOD_SHIFT) != 0 {
		state |= 1
	}
	if mod&int(C.SDL_KMOD_ALT) != 0 {
		state |= 2
	}
	if mod&int(C.SDL_KMOD_CTRL) != 0 {
		state |= 4096
	}
	if mod&int(C.SDL_KMOD_GUI) != 0 {
		state |= 65536
	}
	if mod&int(C.SDL_KMOD_CAPS) != 0 {
		state |= 1048576
	}
	if mod&int(C.SDL_KMOD_NUM) != 0 {
		state |= 2097152
	}
	if mod&int(C.SDL_KMOD_SCROLL) != 0 {
		state |= 4194304
	}
	return state
}

// Printable input is committed by SDL's text/IME events. Only non-printable
// keys and shortcuts get a raw down/up pair, with the down's key saved so a
// modifier or keyboard-layout change cannot strand a different Android key.
func nativeKeyboardInput(scan, mod, symbol int, down, composing bool, held map[int]int) (viewerInput, bool) {
	if !down {
		key, ok := held[scan]
		if !ok {
			return viewerInput{}, false
		}
		delete(held, scan)
		return viewerInput{Type: "key", Key: key, MetaState: nativeMeta(mod)}, true
	}
	shortcut := mod&int(C.SDL_KMOD_CTRL|C.SDL_KMOD_GUI|C.SDL_KMOD_ALT) != 0
	keypadNavigation := mod&int(C.SDL_KMOD_NUM) == 0 && scan >= int(C.SDL_SCANCODE_KP_1) && scan <= int(C.SDL_SCANCODE_KP_PERIOD)
	if composing || mod&int(C.SDL_KMOD_MODE) != 0 || (!shortcut && nativePrintable(scan) && !keypadNavigation) {
		return viewerInput{}, false
	}
	key := nativeAndroidKey(scan)
	if keypadNavigation {
		switch C.SDL_Keycode(symbol) {
		case C.SDLK_UP:
			key = 19
		case C.SDLK_DOWN:
			key = 20
		case C.SDLK_LEFT:
			key = 21
		case C.SDLK_RIGHT:
			key = 22
		case C.SDLK_HOME:
			key = 122
		case C.SDLK_END:
			key = 123
		case C.SDLK_PAGEUP:
			key = 92
		case C.SDLK_PAGEDOWN:
			key = 93
		case C.SDLK_INSERT:
			key = 124
		case C.SDLK_DELETE:
			key = 112
		}
	}
	if key == 0 {
		return viewerInput{}, false
	}
	if shortcut && symbol >= 'a' && symbol <= 'z' {
		key = 29 + symbol - 'a'
	}
	held[scan] = key
	return viewerInput{Type: "key", Key: key, Down: true, MetaState: nativeMeta(mod)}, true
}

func nativePrintable(scan int) bool {
	return scan >= int(C.SDL_SCANCODE_A) && scan <= int(C.SDL_SCANCODE_0) ||
		scan == int(C.SDL_SCANCODE_SPACE) || scan >= int(C.SDL_SCANCODE_MINUS) && scan <= int(C.SDL_SCANCODE_SLASH) && scan != int(C.SDL_SCANCODE_CAPSLOCK) ||
		scan >= int(C.SDL_SCANCODE_KP_DIVIDE) && scan <= int(C.SDL_SCANCODE_KP_PERIOD) && scan != int(C.SDL_SCANCODE_KP_ENTER) ||
		scan == int(C.SDL_SCANCODE_KP_EQUALS) || scan == int(C.SDL_SCANCODE_KP_COMMA)
}

func nativeAndroidKey(scan int) int {
	if scan >= int(C.SDL_SCANCODE_A) && scan <= int(C.SDL_SCANCODE_Z) {
		return 29 + scan - int(C.SDL_SCANCODE_A)
	}
	if scan >= int(C.SDL_SCANCODE_1) && scan <= int(C.SDL_SCANCODE_9) {
		return 8 + scan - int(C.SDL_SCANCODE_1)
	}
	if scan >= int(C.SDL_SCANCODE_F1) && scan <= int(C.SDL_SCANCODE_F12) {
		return 131 + scan - int(C.SDL_SCANCODE_F1)
	}
	if scan >= int(C.SDL_SCANCODE_KP_1) && scan <= int(C.SDL_SCANCODE_KP_9) {
		return 145 + scan - int(C.SDL_SCANCODE_KP_1)
	}
	switch C.SDL_Scancode(scan) {
	case C.SDL_SCANCODE_0:
		return 7
	case C.SDL_SCANCODE_KP_0:
		return 144
	case C.SDL_SCANCODE_BACKSPACE:
		return 67
	case C.SDL_SCANCODE_TAB:
		return 61
	case C.SDL_SCANCODE_RETURN:
		return 66
	case C.SDL_SCANCODE_KP_ENTER:
		return 160
	case C.SDL_SCANCODE_SPACE:
		return 62
	case C.SDL_SCANCODE_ESCAPE:
		return 4
	case C.SDL_SCANCODE_UP:
		return 19
	case C.SDL_SCANCODE_DOWN:
		return 20
	case C.SDL_SCANCODE_LEFT:
		return 21
	case C.SDL_SCANCODE_RIGHT:
		return 22
	case C.SDL_SCANCODE_HOME:
		return 122
	case C.SDL_SCANCODE_END:
		return 123
	case C.SDL_SCANCODE_PAGEUP:
		return 92
	case C.SDL_SCANCODE_PAGEDOWN:
		return 93
	case C.SDL_SCANCODE_INSERT:
		return 124
	case C.SDL_SCANCODE_DELETE:
		return 112
	case C.SDL_SCANCODE_LSHIFT:
		return 59
	case C.SDL_SCANCODE_RSHIFT:
		return 60
	case C.SDL_SCANCODE_LALT:
		return 57
	case C.SDL_SCANCODE_RALT:
		return 58
	case C.SDL_SCANCODE_LCTRL:
		return 113
	case C.SDL_SCANCODE_RCTRL:
		return 114
	case C.SDL_SCANCODE_LGUI:
		return 117
	case C.SDL_SCANCODE_RGUI:
		return 118
	case C.SDL_SCANCODE_CAPSLOCK:
		return 115
	case C.SDL_SCANCODE_NUMLOCKCLEAR:
		return 143
	case C.SDL_SCANCODE_SCROLLLOCK:
		return 116
	case C.SDL_SCANCODE_APPLICATION:
		return 82
	case C.SDL_SCANCODE_MINUS:
		return 69
	case C.SDL_SCANCODE_EQUALS:
		return 70
	case C.SDL_SCANCODE_LEFTBRACKET:
		return 71
	case C.SDL_SCANCODE_RIGHTBRACKET:
		return 72
	case C.SDL_SCANCODE_BACKSLASH:
		return 73
	case C.SDL_SCANCODE_SEMICOLON:
		return 74
	case C.SDL_SCANCODE_APOSTROPHE:
		return 75
	case C.SDL_SCANCODE_GRAVE:
		return 68
	case C.SDL_SCANCODE_COMMA:
		return 55
	case C.SDL_SCANCODE_PERIOD:
		return 56
	case C.SDL_SCANCODE_SLASH:
		return 76
	case C.SDL_SCANCODE_KP_DIVIDE:
		return 154
	case C.SDL_SCANCODE_KP_MULTIPLY:
		return 155
	case C.SDL_SCANCODE_KP_MINUS:
		return 156
	case C.SDL_SCANCODE_KP_PLUS:
		return 157
	case C.SDL_SCANCODE_KP_PERIOD:
		return 158
	case C.SDL_SCANCODE_KP_COMMA:
		return 159
	case C.SDL_SCANCODE_KP_EQUALS:
		return 161
	case C.SDL_SCANCODE_VOLUMEUP:
		return 24
	case C.SDL_SCANCODE_VOLUMEDOWN:
		return 25
	case C.SDL_SCANCODE_MUTE:
		return 164
	case C.SDL_SCANCODE_MEDIA_PLAY_PAUSE:
		return 85
	case C.SDL_SCANCODE_MEDIA_STOP:
		return 86
	case C.SDL_SCANCODE_MEDIA_NEXT_TRACK:
		return 87
	case C.SDL_SCANCODE_MEDIA_PREVIOUS_TRACK:
		return 88
	case C.SDL_SCANCODE_PAUSE:
		return 121
	case C.SDL_SCANCODE_PRINTSCREEN:
		return 120
	}
	return 0
}
