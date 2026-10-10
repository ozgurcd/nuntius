package main

import (
	"fmt"
	"os"
	"os/exec"

	"fyne.io/systray"
)

func desktopSupported() bool {
	return exec.Command("launchctl", "print", fmt.Sprintf("gui/%d", os.Getuid())).Run() == nil
}
func setRelayIcon() {
	normal, _ := assets.ReadFile("assets/nuntiusTemplate.png")
	retina, _ := assets.ReadFile("assets/nuntiusTemplate@2x.png")
	systray.SetTemplateIcon(retina, normal)
}
