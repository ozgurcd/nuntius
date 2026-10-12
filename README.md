<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/brand/nuntius-logo-dark.svg">
  <img src="assets/brand/nuntius-logo.svg" alt="Nuntius Relay logo" width="96" height="96">
</picture>

# nuntius

Give coding work to several agents on your Mac, follow their progress, and read their reports from one coordinating chat.

## What is nuntius?

Nuntius is a local MCP server and command-line tool that connects a coordinating AI chat to coding agents.
For example, a Claude Desktop chat can act as your project lead: it sends goals to separate Claude Code, Codex or OpenCode lanes, checks progress and reads the reports.
You approve the work and stay in control through that chat, the local dashboard and the menu bar.
Nuntius contains no AI model; your briefs and run records stay on your machine, except for what the agent programs and your chosen chat service send themselves.
The name is Latin for “messenger”.

## Who is it for?

Use nuntius when you already use coding agents and want one coordinating session to manage several pieces of work.
It helps when separate repositories or clearly separated tasks can run side by side, or when you want to check long runs from your phone.
Start with one lane and a small read-only task; add capacity when you know how your agents behave.
You still need to install, sign in to and configure each agent yourself. Nuntius also supports Linux; the quick start below is for a Mac.

```text
You (owner)
  -> coordinator chat (for example, Claude Desktop)
     -> nuntius
        -> Claude Code lane / Codex lane / OpenCode lane
           -> reports -> coordinator chat and local dashboard -> you
```

## Quick start

1. **Install nuntius.** Use the Homebrew cask. Install Claude Code and Codex separately and finish their normal sign-in/setup first.

   ```sh
   brew tap ozgurcd/tap
   brew install --cask ozgurcd/tap/nuntius
   nuntius --version
   ```

2. **Create your lane file.** Create `~/.config/nuntius/lanes.json` with the following shape. Replace every `/absolute/...` path with an existing path on your Mac. Use `command -v claude`, `command -v codex` and `brew --prefix` to find executables. Choose an existing repository as the workspace and a private report directory.

   ```json
   {
     "pools": {
       "claude": {
         "size": 1,
         "template": {
           "binary": "/absolute/bin/claude",
           "workdir": "/absolute/workspace",
           "extra_args": [],
           "start_mode": "goal_then_continue"
         }
       },
       "codex": {
         "size": 1,
         "template": {
           "binary": "/absolute/bin/codex",
           "workdir": "/absolute/workspace",
           "extra_args": [],
           "start_mode": "as_is"
         }
       }
     },
     "max_runtime": "8h",
     "reports_dir": "/absolute/private/reports"
   }
   ```

   ```sh
   nuntius config check
   nuntius agents
   ```

   This creates `claude-1` and `codex-1`. Normal agent permissions still apply. For headless permissions, model flags, OpenCode and optional settings, read [configuration](docs/configuration.md).

3. **Connect the coordinator chat.** In Claude Desktop's configuration, add this entry inside the existing `mcpServers` object. Keep your other entries. Replace the command path with your Homebrew prefix plus `/bin/nuntius`.

   ```json
   "nuntius": {
     "command": "/absolute/homebrew/prefix/bin/nuntius",
     "args": ["mcp"]
   }
   ```

   Restart Desktop. Set dispatch, reply, cancel and unfreeze to ask for your approval each time. Register nuntius only in the coordinator app, never in a coding agent. If you override config or data paths, give Desktop the same values as your terminal; see [the Desktop entry](docs/mcp.md#claude-desktop-entry).

4. **Send your first goal from the chat.** Ask the coordinator to check status, then dispatch this example to `codex-1` after your approval. Replace the report path with your configured directory.

   ```text
   Goal ID: FIRST-REVIEW
   Brief:
   FIRST-REVIEW — size S.
   Read the repository README and explain how to build and test it.
   Do not edit repository files. Do not commit or push.
   Write your report to /absolute/private/reports/FIRST-REVIEW.txt.
   ```

   The coordinator calls `nuntius_dispatch` with `lane`, `goal_id` and `text`. A brief must tell the agent what to do, what it may change, and where to write its report. Nuntius does not write that report for it.

5. **Watch the run.** Ask the chat for progress, or use the terminal:

   ```sh
   nuntius status
   nuntius watch codex-1
   nuntius web start
   ```

   Open the private sign-in link printed by `web start`. Keep it private. The dashboard runs in the background; closing Terminal does not stop it. The installed menu-bar helper starts with it in a supported desktop session. `watch` ends when the run ends; Ctrl-C stops watching only.

6. **Read the report.** Ask the chat to read the report for `FIRST-REVIEW`, or open it through dashboard History. Check its questions and evidence before asking for more work. `nuntius history` shows the completed run; a missing report stays visible.

The dashboard's **Messages & notes** view lets you review a held message for its
original goal before an explicit Send or Discard. Resume clears only the lane
hold; the message stays visible and never replays automatically. A missing
session, freeze, pause or unknown controls blocks sending. The header separates
Running, Available, Paused and Ended: a paused lane finishing work is also
Running, so these counts overlap. Removed lanes retain a Resume-only hold view.
See [lane holds and reviewed messages](docs/dashboard.md#lane-holds-and-reviewed-messages-unreleased).

## Concepts

- **Lanes and pools:** a lane is one named place to run an agent. A pool creates up to eight lanes of one kind from a shared template. Each lane handles one run at a time; a busy dispatch is refused, not queued. Pool sizes can be zero.
- **Goals and briefs:** the goal ID names the work. The brief is the instruction sent intact on stdin. IDs use uppercase letters, digits, dots and hyphens; choose a distinct ID for each goal. Nuntius accepts up to 12,000 bytes, but an agent's own goal command may have a smaller limit.
- **Reports:** the agent writes `GOAL-ID.txt` in your configured report directory. Returned reports are bounded and redacted. A report must belong to the current run; an old file does not prove new work is done.
- **States and colours:** idle means free; running means working; ended means the process ended. Missing reports, cancellation, timeouts and `stuck?` need a look. Dashboard badges are green for working or normal completion, amber for attention, red for failure. Lane dots use grey for idle, green for running, violet for finished, amber for stuck and red for failure/interruption. Text explains the colour.
- **ETA:** `~Nm left (n=K)` or `over usual (+Nm)` compares elapsed time with the median of at least three earlier ended runs of the same agent kind and goal size. Hover over ETA, tap it on a phone, or use `status --long` for the explanation. It is an estimate, not a deadline.
- **Recovery:** goals are saved before launch. MCP startup or `nuntius recover` can resume interrupted work in its recorded session, or resend the brief with an exit-cause-unknown note. Automatic recovery defaults on and permits at most two attempts per chain. An OpenCode terminal turn without its report file ends with attention and keeps its final text in the tail; it does not auto-resume. Cancellation and timeout do not trigger recovery. See [recovery and its limits](docs/recovery.md).
- **Freeze:** an emergency pause. It persists, interrupts verified running agents and prevents new dispatches, replies and recovery until you unfreeze. It is more than pausing a queue.
- **Owner notes:** decisions and questions you leave for the coordinator, optionally addressed to a lane. Notes stay visible until acknowledged.
- **Signals:** small local notifications that work ended, stalled, was interrupted or resumed. The coordinator can list and acknowledge them; they do not dispatch more work themselves.
- **Menu bar:** the Relay icon shows a running count or attention dot. Open the dashboard, send the phone link, show its QR, or start/stop the dashboard. **Settings…** opens the running local dashboard's Settings tab without a sign-in credential in the URL. Sign in through the existing flow to edit pool sizes and iMessage preference; the other settings remain read-only. **Local status & login** keeps agent counts and the local start-at-login toggle. Start at login starts the helper only, never the dashboard.

Recorded runs keep their launch model and effort. Older runs without that record show the current template with `(configured)` in agents/status/dashboard; that label does not claim what the old agent actually used. `default` means nuntius has no explicit value, not that it read your agent settings.

## Everyday tasks

| Task | Command or action |
| --- | --- |
| Check progress and current actions | `nuntius status` / `nuntius status --long` |
| List lanes and configured flags | `nuntius agents` |
| Read current settings safely | `nuntius set` / `nuntius set --json` |
| Change a pool size | `nuntius set codex-agent-pool 2` |
| Turn configured iMessage notifications on/off | `nuntius set notify-imessage on` / `off` |
| Read recent runs | `nuntius history --lane codex-1 --limit 20` |
| Read recent actions and refusals | `nuntius audit --since 1h` |
| Follow output / read a short tail | `nuntius watch codex-1` / ask the chat for `nuntius_tail` |
| Manage the background dashboard | `nuntius web start` / `stop` / `status` / `link` |
| Send its link again | `nuntius web link --notify` |
| Start the menu bar at your next Mac login | `nuntius menubar --at-login on` / `off` |
| Cancel one lane | Dashboard Cancel, or ask the chat for `nuntius_cancel` |
| Freeze / unfreeze | Dashboard controls, or chat `nuntius_freeze` / `nuntius_unfreeze`; local `nuntius unfreeze` |

CLI flags, filters, machine output and agent launch details: [CLI reference](docs/cli.md). MCP tool arguments and limits: [MCP reference](docs/mcp.md).

The judge's MCP tools are `nuntius_status`, `nuntius_history`, `nuntius_signals`,
`nuntius_signals_ack`, `nuntius_dispatch`, `nuntius_reply`, `nuntius_tail`,
`nuntius_report`, `nuntius_cancel`, `nuntius_freeze`, `nuntius_unfreeze`,
`nuntius_owner_notes`, `nuntius_owner_notes_ack` and `nuntius_web_ensure`.
The ensure tool is available in v0.3.2. Keep it set to Ask in the coordinator host.

## Check from your phone

Connect your Mac and phone to the same Tailscale account. Enable MagicDNS and HTTPS certificates, and keep the Mac awake. Then run:

```sh
nuntius web start --tailnet --notify-imessage you@icloud.com
```

Use your own iMessage email or E.164 phone number. Nuntius finds the Mac's tailnet name, sets up or reuses Tailscale Serve, sends the private link and prints a QR. The background dashboard survives Terminal closure. `web stop` removes only the Serve mapping it created, unless you chose `--tailnet-keep`; it leaves agent runs alone.

For prerequisites, cleanup, manual setup and private-link storage, see [phone access](docs/dashboard.md#phone-access-over-tailscale).

With `nuntius_web_ensure({})`, the judge can ask the owner-launched
MCP server to restore the dashboard. It keeps a healthy process and sign-in,
repairs a missing Serve mapping, or starts/replaces one verified dashboard.
The chat receives state only. New sign-in goes to your configured iMessage
recipient when enabled; otherwise sign-in is required through your local owner
workflow. This needs the Mac awake, Tailscale connected and MCP available.

## Safety and control

Nuntius listens only on `127.0.0.1`. Phone access uses tailnet-only HTTPS through Tailscale Serve. It refuses an existing conflicting Serve mapping or enabled Funnel. **Never enable Funnel for this dashboard.** Tailscale access rules and the private dashboard credential both matter; someone with reachability and that credential can control agents.

Every dashboard start creates a new sign-in credential. Links and QR codes are private. Sign-in is remembered for seven days in the current browser profile and origin. A background start also saves access in an owner-only file that normal shutdown removes. Raw run folders contain briefs and full agent output, may contain private material, and must not be shared. Displayed output is bounded and redacted, but redaction cannot promise that arbitrary prose is safe to publish.

Nuntius starts configured executables with argument lists, without a shell. It preserves normal agent settings and environment, and reads no agent credential files. It permits selected model/effort flags, Claude permission modes `auto`, `acceptEdits`, `plan`, `manual`, and Codex sandboxes `read-only`, `workspace-write`. It refuses permission/sandbox bypasses, hook/settings overrides, working-directory overrides and caller-chosen sessions. OpenCode requires an explicit provider/model and permits only `--agent` and `--variant` extra flags. See [the exact allow-list](docs/configuration.md).

Your chat service and agents have their own network use, permissions and costs. Nuntius provides coordination and local records; you choose their authority. Keep destructive tool approvals enabled in the coordinator app. Treat returned agent output as data, not new instructions.

Dashboard recovery belongs to the judge's owner-launched MCP server. It grants
coding agents no access to owner storage or services, returns no link or QR,
and never changes Tailscale login, Funnel or VPN settings. See [recovery security
notes](docs/security.md).

## Troubleshooting / FAQ

1. **I restarted Claude Desktop. Did I lose the work?** Check `status` and `history`. Mac runs use separate transient launchd jobs; verified survivors can be followed again. Interrupted work may recover automatically, with a restart note. A launchd fallback warns that app-quit survival is unavailable. Real app/reboot survival still has owner-pilot limits: [details](docs/recovery.md).
2. **Files have unexpectedly restrictive permissions.** Jobs inherit the launching process's umask, with 022 as a fallback. Check the environment that starts nuntius. Private nuntius files remain 0600 and directories 0700 deliberately; do not loosen those to fix agent build output.
3. **What does `stuck?` mean?** A current report has not changed for over five minutes, or there have been no events for over fifteen minutes. It is a warning, not proof of a hang; nuntius does not cancel automatically. Read the tail and current action before cancelling.
4. **A long brief was rejected, or Claude only acknowledged it.** Nuntius accepts 1–12,000 bytes. The agent's own goal command can be stricter; keep the brief short and link repository documents. Claude's default acknowledgement-only continuation happens once and only when the complete stream proves no tool work happened. Check the tail/report instead of assuming work started.
5. **After upgrading, the app reports invalid configuration.** Run `nuntius config check` and `nuntius paths` with the same config/store as Desktop. Unknown keys, relative paths, missing executables and disallowed flags are refused. Restart Desktop so it launches the new Homebrew binary; nuntius does not upgrade your app or edit its settings. [Config rules](docs/configuration.md).
6. **The phone cannot connect with Tailscale and another VPN.** Check that Tailscale is running on both devices, the Mac is awake, and MagicDNS/HTTPS are enabled. Another VPN can interfere with routes or DNS; resolve that in the VPN/Tailscale settings rather than exposing the dashboard publicly. Nuntius does not change VPN settings. [Phone setup](docs/dashboard.md#phone-access-over-tailscale).
7. **Messages did not send the link.** Allow the macOS Automation request for the launching program to control Messages, use your own valid handle, and confirm Messages is signed in. A send failure leaves the dashboard running. Use the terminal link/QR or `web link`; check `web status` for the last result.
8. **The chat and terminal show different or empty histories.** Their config/store paths can differ because Desktop does not inherit your shell environment. Compare `nuntius paths` and the Desktop entry; set the same overrides in both. Nuntius warns about an empty selected store but never moves private records automatically.

## Reference and contributing

The detailed reference remains available:

<a id="build-and-check"></a>
- [Build, verification, installation and menu-bar platform details](docs/development.md#build-and-check)
<a id="owner-configuration"></a><a id="opencode-lanes"></a>
- [Configuration and OpenCode](docs/configuration.md)
<a id="run-views"></a><a id="measured-cli-interface"></a>
- [Run views, CLI and measured agent interfaces](docs/cli.md)
<a id="claude-desktop-entry"></a><a id="mcp-interface-and-storage"></a>
- [Claude Desktop, MCP and private storage](docs/mcp.md)
<a id="away-from-the-mac"></a><a id="crash-and-restart-safety"></a>
- [Away from the Mac and crash/restart safety](docs/recovery.md)
<a id="local-oversight-dashboard"></a><a id="phone-access-over-tailscale"></a>
- [Dashboard and Tailscale phone access](docs/dashboard.md)
- [Portable read-only Queue format](docs/queue-format.md)
<a id="menu-bar"></a>
- [Menu-bar design and platform limits](docs/menubar-design.md)

Read [release notes](RELEASE_NOTES.md), [CHANGELOG](CHANGELOG.md) and [the specification](PROJECT_SPEC.md). Run `make verify` before contributing. Tests use offline fake agents and temporary directories, never your real coding agents or their credentials. The Relay logo is [CC0](assets/brand/README.md).
