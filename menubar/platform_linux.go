package main

import (
	"context"
	"os"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

func desktopSupported() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var owned bool
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&owned)
	return err == nil && owned
}
func setRelayIcon() { icon, _ := assets.ReadFile("assets/relay-linux.png"); systray.SetIcon(icon) }
