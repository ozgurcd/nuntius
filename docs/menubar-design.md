# Menu bar helper (v0.3.2)

The main `nuntius` binary remains CGO_ENABLED=0. The separate `menubar/`
module builds `nuntius-menubar` with fyne.io/systray, maintained by the Fyne
team (a fork of getlantern/systray). Its Apache-2.0/BSD notices ship with
the helper. Cocoa requires cgo and a macOS SDK; Linux uses D-Bus
StatusNotifierItem and does not require GTK. Pin the dependency and build
Darwin on macOS runners; build Linux on Ubuntu runners. No Windows builds.
See [upstream](https://github.com/fyne-io/systray).

The Relay assets are CC0. macOS uses the supplied black/alpha template PNGs
and a background-only `.app` bundle with the supplied app icon. Linux uses
a PNG rasterized from the monochrome SVG (the library accepts PNG, not SVG).
KDE supports StatusNotifierItem; XFCE needs an enabled
StatusNotifier tray plugin. GNOME normally needs an AppIndicator extension.
Without a session bus and registered watcher, print a fixed explanation and
exit successfully. Starting the dashboard never depends on a tray being present.

The helper is not Developer ID signed or notarized. Darwin binaries receive
an ad-hoc signature, which supplies code integrity but no publisher trust.
Homebrew's existing quarantine-removal postflight also covers the helper and
bundle. Manual archive downloads retain Gatekeeper/quarantine rules; the owner
must approve those downloads. Do not disable Gatekeeper globally. Installation
does not launch the helper or enable login startup.

The dashboard hosts a 0600 `dashboard.sock` in its private `menubar` directory.
Relay hosts a separate 0600 `helper.sock` that accepts only quit. These sockets
are not dashboard recovery channels; judge-only recovery uses owner MCP.
Their parent directory is 0700. The dashboard socket exposes bounded, redacted lane
status and explicit open/phone-link/QR actions, never dispatch or cancel.
The server places the current sign-in grant in a private temporary HTML page
only when the user opens the dashboard. It opens the browser itself, so credentials never enter helper
argv, environment, responses or logs. Phone delivery reuses the existing
fixed iMessage path with text on stdin. QR appears in a temporary 0600 local presentation owned by
the dashboard. The helper receives no sign-in link. Socket paths and executable
paths may cross argv; they are not credentials. Start/stop use `nuntius web
start --no-menubar` / `stop`; helper quit leaves the dashboard alive.

The tray controller depends on interfaces. Tests use fake trays, commands,
openers and transports only. Login startup is off by default; an explicit
`nuntius menubar --at-login on|off` manages only the helper LaunchAgent.
No tests load a launchd job, start a GUI, or access the owner's services.

GoReleaser's prebuilt builder requires Pro. Use ordinary archive file entries
to include CI-built helpers and the Darwin bundle; validate each archive and
cask before publishing. The main release remains on ubuntu-24.04.

Settings reads a bounded, safe `set --json` snapshot through the main CLI.
It lists current display values and configured lane identity, not recipients or
flags. A change remains pending until Confirm; Discard clears it. Only pool
sizes, notification on/off and helper-only start-at-login can change.
Open settings file uses Finder reveal on macOS. Settings work while the
dashboard is stopped. Fake-tray tests prove confirmation and CLI argv routing.
