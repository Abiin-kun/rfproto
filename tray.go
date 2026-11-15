package main

import (
	"context"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const trayInterface = "org.kde.StatusNotifierItem"
const trayWatcher = "org.kde.StatusNotifierWatcher"
const trayPath dbus.ObjectPath = "/StatusNotifierItem"

type trayPixmap struct {
	Width, Height int32
	Pixels        []byte
}
type trayTooltip struct {
	IconName           string
	IconPixmap         []trayPixmap
	Title, Description string
}

// DBus callbacks only enqueue actions; SDL remains owned by the event thread.
type systemTray struct {
	conn    *dbus.Conn
	show    chan struct{}
	signals chan *dbus.Signal
}

func newSystemTray(rgba []byte) (*systemTray, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("system tray unavailable: %w", err)
	}
	tray := &systemTray{conn: conn, show: make(chan struct{}, 1), signals: make(chan *dbus.Signal, 4)}
	fail := func(err error) (*systemTray, error) { conn.Close(); return nil, err }
	if err := conn.Export(tray, trayPath, trayInterface); err != nil {
		return fail(err)
	}
	// StatusNotifier pixmaps are network-order ARGB, not SDL's RGBA.
	argb := make([]byte, len(rgba))
	for i := 0; i+3 < len(rgba); i += 4 {
		argb[i], argb[i+1], argb[i+2], argb[i+3] = rgba[i+3], rgba[i], rgba[i+1], rgba[i+2]
	}
	pixmap := []trayPixmap{{64, 64, argb}}
	properties := prop.Map{trayInterface: {
		"Category": {Value: "ApplicationStatus"}, "Id": {Value: "rfproto"},
		"Title": {Value: "rfproto"}, "Status": {Value: "Active"},
		"WindowId": {Value: uint32(0)}, "IconName": {Value: ""}, "IconPixmap": {Value: pixmap},
		"OverlayIconName": {Value: ""}, "OverlayIconPixmap": {Value: []trayPixmap{}},
		"AttentionIconName": {Value: ""}, "AttentionIconPixmap": {Value: []trayPixmap{}},
		"AttentionMovieName": {Value: ""}, "ItemIsMenu": {Value: false},
		"ToolTip": {Value: trayTooltip{"", pixmap, "rfproto", "Click to show rfproto"}},
	}}
	props, err := prop.Export(conn, trayPath, properties)
	if err != nil {
		return fail(err)
	}
	node := &introspect.Node{Interfaces: []introspect.Interface{
		{Name: trayInterface, Methods: introspect.Methods(tray), Properties: props.Introspection(trayInterface)},
		prop.IntrospectData,
	}}
	if err := conn.Export(introspect.NewIntrospectable(node), trayPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return fail(err)
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, trayWatcher)); err != nil {
		return fail(err)
	}
	conn.Signal(tray.signals)
	if err := tray.register(); err != nil {
		return fail(err)
	}
	return tray, nil
}

func (t *systemTray) register() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	watcher := t.conn.Object(trayWatcher, "/StatusNotifierWatcher")
	var host dbus.Variant
	if err := watcher.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, trayWatcher, "IsStatusNotifierHostRegistered").Store(&host); err != nil {
		return fmt.Errorf("no supported system tray: %w", err)
	}
	if registered, ok := host.Value().(bool); !ok || !registered {
		return fmt.Errorf("no system tray host is running")
	}
	if err := watcher.CallWithContext(ctx, trayWatcher+".RegisterStatusNotifierItem", 0, string(trayPath)).Err; err != nil {
		return fmt.Errorf("register tray icon: %w", err)
	}
	return nil
}

func (t *systemTray) requestShow() {
	select {
	case t.show <- struct{}{}:
	default:
	}
}
func (t *systemTray) Activate(x, y int32) *dbus.Error          { t.requestShow(); return nil }
func (t *systemTray) SecondaryActivate(x, y int32) *dbus.Error { t.requestShow(); return nil }
func (t *systemTray) ContextMenu(x, y int32) *dbus.Error       { t.requestShow(); return nil }

// Restore the launcher if its tray host disappears; never strand a hidden UI.
func (t *systemTray) wantsShow() bool {
	select {
	case <-t.show:
		return true
	case signal := <-t.signals:
		return signal != nil && len(signal.Body) == 3 && signal.Body[0] == trayWatcher
	default:
		return !t.conn.Connected()
	}
}
func (t *systemTray) close() { t.conn.RemoveSignal(t.signals); t.conn.Close() }
