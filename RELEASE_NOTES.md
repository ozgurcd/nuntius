# Release notes

User-facing changes and upgrade actions are listed newest first. See the
[CHANGELOG](CHANGELOG.md) for technical detail. Installing an upgrade does not
restart an existing server. Choose when to upgrade through the Homebrew tap
and restart your MCP server or background dashboard to load the new binary.

## Unreleased

- Native admission now explains whether reviewed work is ready and whether a
  lane can start it. Pauses, missing review, unavailable claims and overlapping
  work remain visible reasons to wait. This internal check starts no agent and
  adds no dashboard, CLI or MCP action.

- Oversized native briefs can now use a short pointer to the complete reviewed
  file in an explicitly selected workspace. Nuntius keeps the full text and
  shared rules once, records their digests and sizes, and preserves the file for
  resume. Unsafe or changed files need review. This internal delivery component
  starts no work and adds no dashboard, CLI or MCP control.

- Native history refuses outdated approval after scope changes and approval without a new reviewed decision; no work starts.

- The dashboard shows the Queue count without opening Queue. Idle cards explain
  a missing reason once, and lane settings use plain words. Open **What do these
  counts mean?** for the full explanation; your browser remembers that choice.
  At phone widths and in narrow zoomed windows, the header scrolls out of the
  way and the title and buttons use separate rows. Idle badges are easier to
  read in dark mode. No work starts because of these view changes.

- Native briefs now retain the exact reviewed body and shared rules in an
  immutable seal. Changed source observations need review before approval;
  refreshing a brief removes its old approval and automatic grant. Invalid
  headers, encoding, duplicate shared rules and oversized briefs explain why
  review is needed. These internal operations start no work and remain
  unavailable through the dashboard, CLI or MCP.

- Native prerequisites now require verified work. Cancellation or an ended run
  cannot unblock them. Explicit coverage holds related work until its target
  review is accepted, then closes the covered chain together with visible review
  references. It does not invent runs or reports. These internal operations
  remain unavailable through the dashboard, CLI or MCP.

- Native goal history now checks saved decisions against the work they reviewed,
  using the same rules as a new decision. Old attempts, changed reports and
  unsettled work cannot become verified through history replay. Reasons stay
  on one line, including Unicode separators. This checks history consistency;
  it does not prove who supplied a review reference. These internal controls
  remain unavailable through the dashboard, CLI or MCP.

- Audit readers now check field names before displaying a row. JSON escapes
  are decoded first; known names must match the lowercase schema exactly.
  Case variants and duplicate decoded keys hide that row. The reserved native
  action or a native marker also requires the complete safe native schema, so
  extra private body fields stay out of the audit view. Genuine older rows
  with exact known names remain readable. Removing native evidence is outside
  this rule.

- Native goal decisions now retain why work was parked, cancelled or reviewed.
  A report alone cannot verify work, and a goal decision never stops a running
  agent. Approval explains that brief sealing is still unavailable. Full note
  metadata explains its capacity limit without dropping earlier notes. These
  internal controls are not yet exposed through the dashboard, CLI or MCP.

- Native audit checks now detect missing or damaged delivered events. Native
  work stays held until safe delivery is restored; retry adds only missing
  events and preserves existing history. Files that cannot be checked safely
  explain why. Malformed native rows never expose an unexpected private body
  in the audit view. Genuine older audit rows remain readable.

- Native records now have internal revision checks and retained notes. Creating
  a record leaves it drafted. Permanent results accept notes and correction
  evidence without changing the result. Oversized lists explain the failure
  instead of hiding records. Native controls and launches remain unavailable.

- Native audit delivery now has an internal replayable record. Safe rows show
  which event and adapter they describe; repeated delivery appears once to audit
  readers. Pending delivery keeps native work held. Native goal controls and
  launches are still unavailable.

- Document how native goal changes are saved safely and how to check an
  uncertain save before retrying. Repeating the same operation cannot create
  another history event. This internal component adds no controls or work.

- Document how native goal history is read safely. Incomplete final records
  stay uncommitted; damaged history requires review and is never silently
  skipped or removed. This internal reader adds no controls and starts no work.

- Document the native goal data format: separate work scope, engines and holds.
  This internal component adds no controls and starts no work. Creating goal
  data does not approve it; the portable Queue works as before.

- Review held messages in Messages & notes before sending or discarding the
  exact original message. Resume clears a lane hold and leaves the message
  visible; it starts no work and never sends the message automatically.
  Uncertain responses read the receipt again without claiming delivery.
- Available now means a configured lane without running work or admission
  blockers. Paused includes a running lane that is finishing its current work;
  the header explains that overlap. Removed lanes have a retained-holds section
  with Resume only, and private review rows and drafts clear when you sign out.

- Pause or resume a lane from its Agents card. Pause blocks new work while
  the current run finishes. Resume starts no work and explains remaining holds.
  Unconfirmed saves read current state before you choose another action.
- Agents now explain activity, silence, idle uncertainty and usual duration.
  Attention appears first; cards stay in place while you read or use controls.
  Report links distinguish the current attempt from a previous goal.

- Dashboard reads now show saved lane holds without changing control state or
  starting work. Guarded lane and reviewed-message APIs explain conflicts,
  busy locks and uncertain saves. All dashboard changes share a bounded rate;
  lane buttons use the same guarded API.

- Ask the judge to pause or resume a lane, with your approval on each call.
  Status shows the saved hold and remaining blockers. Pause lets the current
  run finish; resume clears only the hold and starts no work. Failures explain
  what happened and when a fresh status read is needed before trying again.

- Pause a lane from the CLI while its current run finishes. Resume clears only
  that hold and starts no work. List explains saved holds, including removed
  lanes; status keeps failures and reports visible. Held messages stay for your
  explicit review, send or discard. Reading lane state creates no files.

- Reports now put what happened, owner decisions and next steps first.
  Full redacted reports and goals expand below without a second scroll box.
  Unclear answers stay marked as unknown; reported commits are not proof of a push.
- Agent tails keep updating while open. Scroll up to read at your own pace,
  then jump to the latest lines. Private output stays hidden.
- Queue counts recover from stalled connections without a reload. Returning
  to the dashboard refreshes it immediately, including browser history restore.
  Failed refreshes keep the last data and show its age; Settings drafts stay intact.

## v0.3.4 — 2026-10-11

- Recognizable private blocks in agent output stay hidden in activity summaries,
  dashboard data and terminal fallback, including split or nested markers,
  incomplete markers at the end of a file and markers outside the visible window.
- Busy dashboards use much less CPU while refreshing lane activity; cancel
  and freeze still act immediately.
- Signed-out headers use one neutral badge and clear overview counts and refresh
  timestamps, so the previous dispatch state stays private before reload finishes.
- Sign-out, expiry, sign-out in another tab and server rejection immediately
  clear the private freeze tooltip and screen-reader name and navigation counts,
  even while a dashboard action or page reload is pending.
- Settings reads wait briefly when busy and retry within a limit. Specific
  messages distinguish busy reads from missing or invalid configuration. Drafts
  stay intact; saves still refuse busy locks without writing or retrying.
- Idle background maintenance skips ended-run event details and reuses its
  history snapshot. Recovery checks remain active, and explicit status reads
  still include full detail. No upgrade or restart is automatic.
- If a background dashboard cannot start, the terminal now gives the known
  cause and full web.log path. Tailscale sign-in failures, a busy local port,
  another Serve mapping and Funnel no longer produce only “inspect web.log”.
- `nuntius web status` shows the running dashboard version and warns if it
  differs from the current CLI. A dashboard started by an older version may
  report its version as unknown. This command does not restart anything.
- Signed-out dashboard tabs, including Settings, show one clear sign-in panel.
  Open the link from `nuntius web link` or iMessage to sign in.
- Background-start test fixtures now always stop and reap their dashboard
  children, including when startup fails before the dashboard records its state.

## v0.3.3 — 2026-10-11

An unreadable or replaced Queue file now leaves the operational dashboard,
dispatch and reply available. Queue continues to report its own read failure.

**Queue** displays your coordinator's planned goals in priority order, with
Waiting on, Needs owner, search and lane/state filters. Phone rows show the
full waiting details. Queue is read-only: starting work stays with your
coordinator. Any coordinator can publish the documented portable snapshot.
The local owner chooses its file; no dashboard file picker or editor exists.
Failed refreshes clearly label the last known queue, and old snapshots show
a warning. Sign-out clears the queue and ignores late replies. This change
does not install or restart anything.

**Settings** is now a dashboard tab. Review current and pending pool sizes and
iMessage preference, then Save the batch or Cancel your draft. Other settings
remain read-only or local-only. Busy agents keep running, and a stale draft
cannot overwrite a CLI or another tab's change. Sign out stays available during
Save. If a save cannot be confirmed, reload current values before trying again.
The menu-bar **Settings…** item opens this tab in the running local dashboard
without a credential URL; it does not start a stopped dashboard. Helper login
startup stays local in **Local status & login**. Anyone using a signed-in phone
profile can change these persistent preferences; protect that profile.

The dashboard Audit list hides read-only polling and inspection entries by
default. Check **Show reads** to include them again; your browser remembers
the choice. Text, lane and action filters still combine, and the count shows
matching entries from the latest received audit rows. Global entries show
**all lanes**, and missing goals or briefs show **n/a**. The complete audit
log and dashboard access rules stay unchanged. No restart or upgrade is automatic.

Offline Settings checks also cover stale requests and content-based revisions.
Test-only maintenance removes polling contention without changing runtime behavior.

## v0.3.2 — 2026-10-10

Sign out stays available while a dashboard action is waiting for a response.
It forgets this browser's sign-in immediately and reloads the public page.
A late action response cannot sign you back in or restore dashboard data.
No upgrade or restart is automatic.

If dashboard startup fails after creating its Tailscale HTTPS mapping, Nuntius
now removes that mapping only when it still points exactly to its own port.
Foreign or uncertain mappings stay untouched, and cleanup failures are reported.
Recovery safety checks have additional offline regression coverage. Judge-only
recovery relies on MCP registration and OS user authority, not authentication
of the stdio caller. This fix changes no lane permissions or owner settings.

After first sign-in, bookmark the cleaned dashboard address on your iPhone.
Sign-in is remembered in that browser profile for seven days, so tab discard
and reload no longer require finding the original message. Use Sign out in
the top bar to forget this browser and clear the page. To revoke every device
and old link, stop the dashboard and start it again when ready. Use the helper
or `nuntius web link --notify` for a fresh sign-in link after expiry or restart.
Use only a trusted private browser profile; anyone using that unlocked profile
can access the bookmark. Cleared/disabled storage or private browsing may
require the original link again. Tailnet-only access and API guards are unchanged.
No upgrade or restart is automatic.

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
owner MCP. Upgrade through the Homebrew tap only when you choose;
no upgrade or restart is automatic.

OpenCode work that ends its final turn but fails to write the report now shows
`Ended · report missing` with amber attention. Read its final answer and parked
questions in the tail. Nuntius does not replay that work automatically or treat
the missing report as success. Interrupted work still uses the existing bounded
recovery policy; recovery notes say when the process exit cause is unknown.
No configuration change is needed.

Offline checks also cover safe mapping readback and the expected recovery tool.
Test-only maintenance shortens a temporary working directory and synchronizes
fixture settings; runtime behavior is unchanged.

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
