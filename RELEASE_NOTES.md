# Release notes

User-facing changes and upgrade actions are listed newest first. See the
[CHANGELOG](CHANGELOG.md) for technical detail. Installing an upgrade does not
restart an existing server. Choose when to upgrade through the Homebrew tap
and restart your MCP server or background dashboard to load the new binary.

## Unreleased

After first sign-in, bookmark the cleaned dashboard address on your iPhone.
Sign-in is remembered in that browser profile for seven days, so tab discard
and reload no longer require finding the original message. Use Sign out in
the top bar to forget this browser and clear the page. To revoke every device
and old link, stop the dashboard and start it again when ready. Use the helper
or `nuntius web link --notify` for a fresh sign-in link after expiry or restart.
Use only a trusted private browser profile; anyone using that unlocked profile
can access the bookmark. Cleared/disabled storage or private browsing may
require the original link again. Tailnet-only access and API guards are unchanged.
This change is unreleased; no upgrade or restart is automatic.

The judge can use `nuntius_web_ensure` to ask the owner-launched MCP server to
restore phone dashboard access. Set this tool to Ask for each call in the host.
A healthy dashboard keeps its sign-in; a missing Tailscale Serve mapping is
repaired without restarting. A missing or verified unhealthy dashboard can
start again with required tailnet access. Unsafe or unavailable state is refused.
The chat receives only status. A new sign-in link goes privately to your enabled,
configured iMessage recipient; failed or disabled delivery leaves web running
and reports that sign-in is needed. Existing sign-in is not resent.

Keep the Mac awake and Tailscale connected. This does not add automatic login
startup, restart supervision or a recovery HTTP endpoint, and does not change
Funnel, VPN settings or coding-agent permissions. It cannot recover a stopped
owner MCP. This tool is awaiting owner-approved release and installation;
no upgrade or restart is automatic.

OpenCode work that ends its final turn but fails to write the report now shows
`Ended · report missing` with amber attention. Read its final answer and parked
questions in the tail. Nuntius does not replay that work automatically or treat
the missing report as success. Interrupted work still uses the existing bounded
recovery policy; recovery notes say when the process exit cause is unknown.
No configuration change is needed. This change is awaiting the next release.

## v0.3.1 — 2026-10-10

Start with a plain-English guide to what nuntius does, who it helps and how to
run your first goal. The complete reference now lives in linked docs, and old
README anchors still lead to those pages.

- Use `nuntius --version` or `-v` as well as `version`.
- Hover over ETA or tap it on a phone to see what “usual” means. The short
  estimate stays unchanged; `status --long` also explains it.
- Older runs without a recorded model/effort show your current template with
  `(configured)` in agents/status/dashboard, so it is clear what is known.
- Scroll wide per-agent history tables on a phone; long tail paths wrap.
- Inspect current settings from the menu bar. Confirm pool-size, iMessage
  on/off and start-at-login changes; reveal the settings file in Finder.
- Resumed runs without reports retain their attention indicator (the fix
  after v0.3.0 is included).

Upgrade with the Homebrew cask when you choose, then restart Claude Desktop
and the dashboard/helper to use the new binary. No owner upgrade is automatic.
Existing config remains valid; notification toggles preserve your recipient.

## v0.3.0 — 2026-10-10

`nuntius web start` now starts the separate Relay menu-bar helper when installed
in a desktop session. Its icon shows idle, the running lane count, or attention
for stuck, failed, unreported or question-waiting work. Hover for counts. The
menu lists each lane, goal, state and elapsed time. Open the signed-in dashboard,
send the configured phone link, show its QR, or start/stop the dashboard.
Quit menu bar leaves the dashboard running. Dispatch/cancel stay on the
dashboard with confirmations. Use `web start --no-menubar` to skip the helper.

Start-at-login is off by default. On macOS, `nuntius menubar --at-login on`
installs a helper-only LaunchAgent for your next login; `--at-login off` removes
it. It never starts the dashboard at login. Linux needs a StatusNotifier tray:
KDE, XFCE with its plugin, or GNOME with an AppIndicator extension. Without a
supported tray the helper exits successfully and the dashboard still works.

Agents lane tabs now have distinct dots for idle, running, finished, stuck and
failed/interrupted work. Hover for the state; screen readers receive the same
label. The tab itself keeps its normal styling.

The main binary stays pure Go. The macOS helper uses Cocoa and ships as an
ad-hoc signed, unnotarized app in the same Homebrew cask. Homebrew handles
quarantine for installed files; manual downloads still need normal Gatekeeper
approval. Upgrade when convenient and restart your dashboard to load this
version. An upgrade does not touch an already running dashboard.

## v0.2.11 — 2026-10-10

See each agent's model and effort directly in `agents`, `status --long`, history
and the dashboard. Claude and Codex model flags now show their values. Codex
effort shows the agent's structured startup value when available, or `default`
when its stream omits that value. Nuntius does not inspect your agent settings.
Historical runs keep their recorded launch values after you change a template.

Use `nuntius audit --since 1h` to read recent actions, or add `--json` for tools.
The dashboard Audit view has readable rows and lane/action filters; Raw reveals
the redacted original record. Agents now has an All view and a tab for each
lane, including its live output, recent history, report links, notes and signals.
You can bookmark a lane, for example `#agents/codex-2`. Select an agent in the
owner-note form to keep that note with its lane.

Upgrade through the Homebrew tap when convenient, then restart your MCP server
or dashboard to load the new views. No agent configuration change is required.
This guide is included in archives; release checks require a matching section.

Details: [CHANGELOG — v0.2.11](CHANGELOG.md#v0211--2026-10-10).

## v0.2.10 — 2026-10-10

Run `nuntius web start` to open a background dashboard without keeping a
terminal open. Use `web status`, `web link` and `web stop` to manage it;
`web link --notify` sends the sign-in link through your configured notification
path. Foreground `web` still works. `set` now lists pool sizes or reads one
setting. A new run no longer inherits the report status of an earlier run of
the same goal. Terminal QR codes and dashboard branding are clearer.
Choose when to restart an existing dashboard after upgrading.

Details: [CHANGELOG — v0.2.10](CHANGELOG.md#v0210--2026-10-10).

## v0.2.9 — 2026-10-10

Use `nuntius web --tailnet` for phone access through Tailscale. Nuntius checks
MagicDNS and HTTPS support, creates or reuses a compatible Serve mapping,
and explains what to enable when setup is incomplete. It refuses conflicting
Serve mappings and Funnel. `--tailnet-optional` allows a local-only fallback.
Use `--notify-imessage` with your own configured handle to receive the private
sign-in link; macOS may request Messages permission. Mappings created by the
run are removed on exit unless you choose `--tailnet-keep`. Keep the Mac awake.

Details: [CHANGELOG — v0.2.9](CHANGELOG.md#v029--2026-10-10).

## v0.2.8 — 2026-10-10

Dashboard colours now follow the run outcome consistently, including resumed
runs. Status and history name a resumed run's finished, failed, cancelled or
timed-out outcome. Green means working or finished normally, amber means
needs a look, and red means failed. No configuration changes are needed.

Details: [CHANGELOG — v0.2.8](CHANGELOG.md#v028--2026-10-10).

## v0.2.7 — 2026-10-10

Phone access can use one exact Tailscale hostname through `web --allow-host`.
The terminal prints sign-in links and a QR code. Follow the README's Tailscale
Serve instructions and keep the Mac awake; this version does not configure
Tailscale for you. Resumed active runs show their recovery count. macOS agent
jobs inherit the launching process's file-creation mask while run files stay
private. No agent configuration change is needed.

Details: [CHANGELOG — v0.2.7](CHANGELOG.md#v027--2026-10-10).

## v0.2.6 — 2026-10-10

OpenCode is now available as a lane or pool. Select a provider/model explicitly
in its template, then use the same dispatch, follow, resume and cancel controls.
Its activity appears in the CLI, MCP and dashboard. Read the README's permission
and guard setup pointers before adding a real OpenCode lane. Existing Claude
and Codex lanes need no changes. OpenCode compatibility is covered by offline
fixtures; it has not been confirmed against live model output.

Details: [CHANGELOG — v0.2.6](CHANGELOG.md#v026--2026-10-10).

## v0.2.5 — 2026-10-10

Watch, MCP and dashboard tails show readable commands, paths and task actions,
with returned text redacted. `nuntius status --long` shows the current action,
its age and unfinished-command duration when available. History groups related
continuations and recovery runs and uses short run IDs. Existing CLI JSON
types and full IDs remain compatible. No configuration changes are needed.

Details: [CHANGELOG — v0.2.5](CHANGELOG.md#v025--2026-10-10).

## v0.2.4 — 2026-10-10

macOS agents start in temporary per-run launchd jobs outside the dispatching
app's job. Linux keeps detached starts. No login item or permanent service is
installed. If launchctl fails, Nuntius reports the fallback instead of hiding
it. Cleanup verifies the recorded job and process identity. The owner's live
app-quit survival check is still needed; offline tests cannot prove that pilot.

Details: [CHANGELOG — v0.2.4](CHANGELOG.md#v024--2026-10-10).

## v0.2.3 — 2026-10-10

Agent runs detach from their parent, and interrupted work is recorded before
launch. Startup recovery or `nuntius recover` can resume a recorded session
or resend the brief with a restart note. Automatic recovery defaults on and
is limited to two attempts per chain. Frozen dispatch, cancellation and timeouts
prevent recovery. Review the linked history after an interruption; detached
exit codes that cannot be collected remain unknown.

Details: [CHANGELOG — v0.2.3](CHANGELOG.md#v023--2026-10-10).

## v0.2.2 — 2026-10-09

Use `nuntius history` or the read-only `nuntius_history` MCP tool to inspect
finished runs and filter missing reports, cancellations and timeouts. Status
marks runs that may be stuck without cancelling them. The dashboard adds
responsive tabs, history filters, readable times and visible data errors,
and retains your focused controls while refreshing. Phone sign-in survives
ordinary reloads in the same tab. No configuration changes are needed.

Details: [CHANGELOG — v0.2.2](CHANGELOG.md#v022--2026-10-09).

## v0.2.1 — 2026-10-09

`nuntius status` now shows aligned human-readable progress, activity and ETA.
Use `--json` for the existing machine-readable format. `nuntius paths` explains
which config and data locations are selected. Run-end signals help you notice
finished work; notifications are skipped outside macOS. Review the README's
app environment guidance if the app and terminal use different data paths.

Details: [CHANGELOG — v0.2.1](CHANGELOG.md#v021--2026-10-09).

## v0.2.0 — 2026-10-09

Runs can survive a server restart with supervision restored after checking
their process identity. Use freeze, owner notes and acknowledgements to direct
work. The loopback dashboard adds oversight and queued replies with per-start
sign-in. Pool sizes can be changed through owner commands; configuration
conversion is explicit. Keep MCP actions subject to the host's owner approval.
See the README before converting existing lane configuration to pools.

Details: [CHANGELOG — v0.2.0](CHANGELOG.md#v020--2026-10-09).

## v0.1.2 — 2026-10-09

Public macOS and Linux downloads include checksums for Intel and ARM machines.
Install through `brew install --cask ozgurcd/tap/nuntius`; do not install into
your Go bin directory. Runtime behaviour remains the same as v0.1.1.
Choose when to upgrade and restart your MCP server yourself.

Details: [CHANGELOG — v0.1.2](CHANGELOG.md#v012--2026-10-09).

## v0.1.1 — 2026-10-09 (not tagged)

This version was recorded in the changelog without a release tag. Claude lanes
can select the supported normal permission modes. Codex lanes can skip the
repository check and select read-only or workspace-write sandbox modes. Other
argument restrictions remain in place. Existing settings are never rewritten;
choose the permitted flags in your own lane configuration if needed.

Details: [CHANGELOG — v0.1.1](CHANGELOG.md#v011--2026-10-09-not-tagged).

## v0.1.0 — 2026-10-09 (not tagged)

This initial version was recorded without a release tag. Nuntius lets the judge
start, follow, resume and cancel Claude and Codex runs through a local MCP server.
Private run storage and redacted, bounded returned text support oversight.
Busy lanes and permission or hook overrides are refused. Register the MCP
server only in the judge's host, and configure owner-approved agent lanes.

Details: [CHANGELOG — v0.1.0](CHANGELOG.md#v010--2026-10-09-not-tagged).
