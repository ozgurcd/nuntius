## Local oversight dashboard

### Queue (since v0.3.3)

Open **Queue** or bookmark `/#queue` after signing in. It displays planned
goals in coordinator order, with size, eligible lane kinds, preferred lane,
state and Waiting on. Needs owner is an independent amber hold with its reason;
an absent prerequisite is labelled outside this queue, never completed.
Ready uses a blue badge and does not mean an agent is free or work approved.
There are no approval, planning, dispatch or repair controls.

Search ID/title, lane kind, state and Needs owner filters combine. Clear filters
restores the list; filtering preserves original priorities and the tab's total
count. Phone rows become labelled cards with wrapping IDs and full waiting text.
Keyboard Arrow/Home/End navigation works across tabs.

Queue reads once on entry and every ten seconds while visible and signed in,
independently of the two-second operational poll. Filters, focus and expanded
details remain in memory through refreshes. Snapshot publication time and last
successful fetch time are separate. Failed refreshes show the last valid queue
with a Last known queue banner and the age of the last successful read. Snapshots older than fifteen minutes show an
old-snapshot warning. Missing default, disabled, missing override, invalid data,
published-empty and no-match states are distinguished. Sign-out, expiry and
cross-tab changes clear queue data; late responses cannot restore it. No queue
data or filters enter browser storage.

Dashboard, Queue and Settings reads have a ten-second client deadline covering
both the request and JSON decoding. A stalled read cannot block later polls.
Returning to a visible tab, focusing the window or restoring the page from
browser history refreshes operational data and the selected polled tab at once.
Events in the same turn share one refresh. New reads replace obsolete ones;
older replies and failures cannot overwrite the latest result. Sign-out,
expiry and leaving the page abort pending reads. Browser suspension can delay
timers; returning to the page starts a fresh read. Mutation requests are never
retried automatically.

Any coordinator can publish the [portable snapshot format](queue-format.md).
The local owner selects `queue_file`; the dashboard never exposes or edits it.

### Settings (since v0.3.3)

Open **Settings** or bookmark `/#settings` after signing in. Pool sizes for
Claude, Codex and OpenCode accept whole numbers 0–8; zero disables an existing
pool without deleting its template. Unconfigured pools cannot be enabled here.
Legacy lanes require local `nuntius config convert` when idle. The iMessage
switch changes only the enabled preference, never the private recipient.
Enabling requires a recipient already configured on the Mac.

Current values, pending deltas and a review list precede **Save changes**.
Save validates every field and busy/surviving agents before one atomic private
config write. Busy shrink refuses the entire batch. New runs use new settings;
running agents and historical launch identities keep theirs. Cancel restores
the last received values. Drafts stay in memory across tabs and operational
polling; they are never saved in browser storage. Visible Settings polls every
ten seconds. Re-entry uses the same cadence; foreground restoration and Reload
refresh immediately. Reload updates current values but keeps a
draft for review. A stale revision cannot overwrite another tab or CLI change.

Runtime, resume, desktop notification, paths, models/effort, lane identities,
template execution options, helper login status and startup/network options
are marked read-only or local-only. Raw arguments, continuation text and the
iMessage recipient are never sent to the browser. Config/store paths and
listener/host/notification overrides are chosen at startup. There is no file
opener, generic config editor, model discovery or restart control.
The start-at-login toggle stays local in **Local status & login** in the menu.

iMessage preference is used when notification settings are next loaded.
An existing dashboard's captured delivery settings/CLI override may require a
local restart; saving sends no link and performs no restart. Settings… opens
the running local dashboard without a credential URL; a signed-out browser
uses the existing sign-in flow. A stopped dashboard must be started locally.

Sign out stays usable during Save, clears private settings/drafts immediately,
and ignores late responses. Expiry and cross-tab sign-out do the same.
Failures preserve drafts. An unconfirmed save or committed-audit failure asks
you to reload current values before retrying; never assume a rollback.
Reads wait briefly for busy locks. A busy response shows "Settings busy,
retrying" and retries up to three times while the tab remains visible and
signed in. Reload current values permits a manual retry immediately. Missing
or invalid configuration gives a specific reason and asks you to run
`nuntius config check` locally. Saves refuse busy locks without writing or
automatic retries.
See [the persistent-write threat model and audit limits](security.md#dashboard-settings-v033).

### Judge-only dashboard recovery

Since v0.3.2: `nuntius_web_ensure({})` runs inside the existing owner-launched
stdio MCP server. Register that server for the judge only and set this tool to
Ask for each call. It takes no configuration or executable arguments.

The operation uses the dashboard lifecycle lock shared with CLI/helper start
and stop. Busy returns immediately. It distinguishes missing state from
unreadable, malformed or linked state, checks PID plus kernel start identity,
and preflights Tailscale with fixed, bounded commands. Running, an exact
MagicDNS name in the certificate domains, no Funnel on any port and an absent
or exact HTTPS 443 mapping are required. Foreign or unavailable configuration
is refused; the tool never logs in, resets, installs or changes VPN settings.

A healthy dashboard with the exact host policy keeps its process and grant.
A missing mapping is repaired without restarting. An absent dashboard starts
in required tailnet mode, without loopback fallback. One identity-verified
unhealthy or incompatible process may be replaced gracefully. An uncertain
stop remains pending; no force-kill, port-based kill or duplicate start occurs.
The absent dashboard uses the fixed default `127.0.0.1:8123`; a validated prior
dashboard record retains its owner-selected loopback port.

Readiness uses a local authenticated `/data` request with the credential only
in its header and the exact tailnet Host. Redirects, environment proxies and
response-body logging are disabled. Mapping writes are read back. Repairs
record private identity-bound ownership, and cleanup reads Serve again before
removing an exact mapping. External Tailscale changes cannot be made atomic;
readback failure stays visible and foreign mappings are left alone.

The result fields are `local` (unknown/ready/pending), `operation`
(none/started/restarted/serve_repaired), `tailnet` (unknown/absent/ready),
`notification` (disabled/sent/failed), `sign_in_required` (boolean),
`remote_client` (always unknown), and a stable `reason`. No private paths,
recipient, token, link, QR, command output or file content are returned.
Local readiness and exact Serve configuration do not prove phone connectivity.

After a new grant, the enabled configured iMessage recipient receives one
private sign-in link through the fixed Messages stdin transport. Healthy reuse
does not resend. Disabled or failed delivery leaves the healthy dashboard up
and reports that sign-in is required. There is no chat-link fallback or resend
tool. The operation is bounded to two minutes, commands to five seconds,
probes to two seconds and notification to ten seconds, without background retries.

The Mac must be awake and logged in, and the owner MCP and Tailscale must be
available. This adds no KeepAlive, login item, watchdog, helper recovery socket
or unauthenticated HTTP recovery route. Freeze and agent goal controls are
unchanged. Offline tests use temporary state and fake transports; owner phone
recovery and authenticated access remain separate pilot checks.

```sh
nuntius web
```

Open the private URL printed by that invocation. It contains a new random
credential each start; do not share or log the URL. The page removes it from
the address bar after loading and remembers it in `localStorage` for this
origin and browser profile for seven days (since v0.3.2). Reload and opening a
bookmark after the phone discards the tab keep sign-in and the selected view.
They do not extend the expiry. If storage is disabled or cleared, the start
link works for the current load, but reload needs that link again. Without a credential the page
shows: "Open the link printed by `nuntius web` to sign in."
Binding is restricted to `127.0.0.1`; use
`nuntius web --bind 127.0.0.1:8124` for another port. No LAN/remote bind or CORS
is enabled. The static shell (`index.html`, `app.css`, `app.js`) is available
without sign-in and contains no private data or credential. Every request still
checks the allowed Host and port, method and security headers; POST Origin must
match the request's local origin, or the exact opted-in HTTPS origin below. `/data` and `/action` require the
`X-Nuntius-Token` header; a query parameter cannot authenticate an API.
Actions accept only POST.

Every signed-out tab, including Settings, shows one sign-in panel instead of
private content, editable forms or connection controls. Open the link from
`nuntius web link` or iMessage to sign in. Tab navigation does not request
private data while signed out; expiry and revocation return to the same panel.

Use **Sign out** in the top bar to forget sign-in in this browser and clear
the displayed private data and drafts. Other open tabs at this origin also
sign out. This does not stop agent runs or revoke the link on other devices.
To revoke all links and remembered sign-ins, run `nuntius web stop`, then
start again when ready; the new start uses a new grant. Sign-out and expiry
need the original link again: use the menu-bar helper's dashboard link or
`nuntius web link --notify` to obtain it privately. A server rejection clears
the saved sign-in; a temporary network failure keeps it for retry.
Remember sign-in only on your own trusted browser profile. Anyone using that
unlocked profile can open the bookmark. See [security notes](security.md).

### Phone access over Tailscale

Install and connect Tailscale on the Mac and iPhone with the same account.
Enable MagicDNS and HTTPS certificates for the tailnet. See the official
[Serve guide](https://tailscale.com/docs/features/tailscale-serve) and
[HTTPS setup](https://tailscale.com/docs/how-to/set-up-https-certificates).
The Mac must stay awake and connected to Tailscale. The background dashboard
survives closing Terminal; it is a transient per-user job, not a login item.

One command does the rest:

```sh
nuntius web start --tailnet --notify-imessage you@icloud.com
```

`start` waits for the loopback dashboard to answer, prints one summary, the
private sign-in link and a compact terminal QR, then exits. On macOS it uses
the per-user launchd job `gui/<uid>/nuntius.web`, with no RunAtLoad or KeepAlive
and your inherited umask. On Linux it starts a detached session with a private
`web.pid` in the data directory. Standard input is closed; stdout/stderr go to
the private `web.log`, which never receives the sign-in link or QR.
If startup fails, the terminal prints the known cause and full log path
(by default `~/Library/Application Support/nuntius/web.log` on macOS).
Diagnostics distinguish a missing/disconnected Tailscale CLI, unavailable
status, an occupied local port, a conflicting Serve mapping and enabled Funnel.
The parent receives only a known failure code; it does not copy log contents.
An existing dashboard is left running; use `web start --replace` to restart it.

```sh
nuntius web status
nuntius web link
nuntius web link --notify
nuntius web stop
```

`status` and `status --long` show the same credential-free dashboard line:
running state and dashboard version, start time, PID/job, port, tailnet host,
Serve mode and last iMessage result. A version mismatch warns with the current
CLI version. An older dashboard record shows `version=unknown` and a warning;
the installed CLI cannot infer the version of that already running process.
Status does not restart or upgrade anything.
`link` displays the current link and QR again; `--notify`
resends to the handle used at start. `stop` sends a clean shutdown, removes
the job and private access files, and succeeds when nothing is running.
It does not stop agent runs. Plain `nuntius web` keeps foreground behaviour.

Background access is stored only in `web-access.json` (0600) inside your
private data directory (0700), so only your OS account can read it. It contains
the current credential, handle and process identity; keep it private and out
of backups or shared folders. It is removed on normal stop and replaced on
restart. A crash can leave a stale private file; `web stop` removes it safely.
Foreground mode keeps its credential in memory and has no retrievable file.

The QR uses qr.L, one character per module column and two module rows per
half-block line, black on white with a two-module quiet zone. If the known
terminal width is too small, only links print with a fit warning.

`--tailnet` finds the Tailscale CLI (`tailscale` on PATH, else the one inside
`/Applications/Tailscale.app`), reads `tailscale status --json`, and requires
Tailscale to be running with a MagicDNS name for this Mac that is in its HTTPS
certificate domains. Each missing piece stops the start with one sentence that
says what to turn on (log in, MagicDNS, HTTPS certificates). Add
`--tailnet-optional` to start the plain loopback dashboard instead when that
check fails. `--allow-host` still works; when both are given the name must
match the detected one.

What nuntius changes in Tailscale, and undoes: it reads `tailscale serve
status --json`. If `https:443` already proxies to this dashboard's loopback
port, it reuses that mapping and leaves it alone. Otherwise it runs
`tailscale serve --bg --https=443 localhost:PORT` with the dashboard's port and
confirms the mapping with serve status. On a normal exit (Ctrl-C or SIGTERM)
it re-reads Serve and removes only a still-exact mapping it created in this run with
`tailscale serve --https=443 off`; `--tailnet-keep` leaves it in place. If 443
serves anything else, nuntius does not touch it and prints the exact
`tailscale serve --https=443 off` command you can run yourself. If Funnel is on
for this Mac, nuntius refuses tailnet mode and prints the
`tailscale funnel --https=PORT off` command; nuntius never enables or disables
Funnel and calls no other tailscale command that changes state.

`--notify-imessage HANDLE` (or the `notify_imessage` config key) sends one
iMessage, `nuntius dashboard (mac): https://mac.tail123.ts.net/?token=…`, to
your own Apple ID (an email address) or phone number (E.164) through
`osascript` and the Messages app, after serve is confirmed. The handle and
text go through stdin to a fixed AppleScript, never process arguments. The first run asks for
macOS permission for your terminal to control Messages; allow it. If sending
fails, nuntius prints a warning and the dashboard keeps running; the terminal
link and QR code still print. The link is never written to a nuntius log or
history. Only the background owner-only access file described above stores its
credential. Start prints one summary without the credential: local URL,
tailnet host, Serve mode and iMessage result. `web status` shows the last send.

Security model: the dashboard stays tailnet-only. Nuntius listens only on
`127.0.0.1`; Tailscale Serve supplies HTTPS to tailnet devices and nuntius
does not validate tailnet membership, so Tailscale's access rules and the
dashboard credential both matter. Every start mints a new credential, so an old
link or message stops working when `nuntius web` restarts. Anyone who can reach
the dashboard and has its credential can dispatch and cancel agents:
**never enable Tailscale Funnel**, and keep the link and QR private.

Manual fallback. Find the Mac's full DNS name in Tailscale, for example
`mac.tail123.ts.net`, and start the dashboard with that exact name (one
`--allow-host` value):

```sh
nuntius web --allow-host mac.tail123.ts.net
```

Nuntius still listens only on `127.0.0.1:8123`. In another Terminal, use the
dashboard port shown in the loopback link (8123 by default):

```sh
tailscale serve --bg --https=443 localhost:8123
tailscale serve status
```

On the iPhone, scan the terminal QR code or open the printed private
`https://mac.tail123.ts.net/` sign-in link. The link and QR contain the same
per-start credential; keep both private. The plain loopback sign-in link also
remains available. After first sign-in, bookmark the cleaned address in the
same iPhone browser profile. It works after tab discard or reload for seven
days, unless browser storage is cleared or the dashboard restarts. Sign-in
uses origin-scoped `localStorage`; API credentials are sent only in the
`X-Nuntius-Token` header. Private browsing may clear storage when closed.

The opt-in accepts only the exact DNS Host, with no port or `:443`, and exactly
`https://mac.tail123.ts.net` as its POST Origin. Wildcards, other hosts and
ports, HTTP origins, and forwarded headers grant no access. To turn off a
manual Serve endpoint:

```sh
tailscale serve --https=443 off
```

See the official [Serve command reference](https://tailscale.com/docs/reference/tailscale-cli/serve).

The dashboard adapts to laptop and phone-width windows and your system's light
or dark mode. The sticky top bar shows dispatch/freeze controls, running, idle
and ended counts, and refresh freshness on every tab. Agents opens by default;
Messages & notes, Digest, Reports & runs, Audit and Settings have their own panels. Arrow keys
and Home/End navigate tabs. The URL hash selects the view on start and reload.
The phone header uses two lines: brand and freeze controls, then counts and
update age. Its pill shows `Enabled` / `Frozen` at narrow widths, with full
dispatch/freeze text in its title and accessible label. The tab bar scrolls
within phone-width windows.
Compact agent cards show kind, state, short run ID, goal, elapsed time, ETA, event age, current action and
report presence. Open a card's tail to read its bounded output. Cancel uses
an in-page confirmation before interrupting a running agent; Keep running or
the timeout restores the initial button. Message opens that agent's composer.

Within Agents, choose All or one lane. Lane tabs show the server's badge colour
and scroll horizontally on a phone. A lane link such as `#agents/codex-2`
survives reload and back/forward navigation. Pool changes update the tab strip;
removing the selected lane returns to All. A lane view includes model/effort,
its live tail with newest output at the bottom (automatic scrolling can be paused), its rows from the latest 50
dashboard runs with report links, lane-specific owner notes and pending signals.
Use the note form's For selector to address a lane; shared judge notes stay in
Messages & notes.

The other tabs show messages and notes, the judge's `OVERSIGHT.md` digest next
to the reports directory (or optional absolute `oversight_path` in config),
the latest 50 runs and the bounded audit log. Audit rows show action, lane, goal,
result/refusal reason and short brief digest, newest first. Read-only actions
(status, report, signals, history, tail and owner-note reads) are hidden by
default. Check **Show reads** to include them; this preference is remembered
per browser origin and profile. If storage is disabled, the checkbox still
works for the current page. State-changing and unknown actions stay visible.
Filter by text, lane or action; filters combine with Show reads, and the count
shows matching entries out of the latest bounded audit entries received.
An empty lane shows **all lanes**; missing goals and brief digests show muted
**n/a**. Each Raw toggle shows the original redacted JSON. Filtering changes
only the display: the complete server audit log and its read entries remain
unchanged. Read classification comes from the existing MCP tool annotations.
Filter history by goal ID or lane,
then expand a row for the report's exact plain summary, status, owner questions,
reported commits and next steps. Process outcome stays separate from reported
completion. Missing, repeated, conflicting, redacted and truncated sections keep
their uncertainty visible. Full redacted reports and original goals use disclosures
and page scroll. The list describes only the latest received attempts, up to 50;
repeated questions in resumed attempts are counted separately. Report association
uses the existing report timestamp checks, not native attempt metadata.
Times are relative with absolute dates on hover.
History rows stack on phones without splitting badge, lane or duration words.
Badge colours come from the server's `badge` field, never from the label text:
green = working or finished normally (including a resumed run), amber = needs
a look (no report, cancelled, timed out, `stuck?`), red = failed. Idle and
interrupted badges stay plain.
An OpenCode run that ended its final turn without delivering the report shows
`Ended · report missing`, an amber badge and an amber lane dot labelled
`Finished with attention: report missing`. Read its final text in the tail;
the report link remains unavailable and the run is not completed or resumed.
Tab counts show running agents, unread owner notes and runs with open questions.

Polling runs every two seconds; hidden panels update when shown. Stable cards and rows keep focus and expansion;
reading an open report or audit log pauses its content updates. Open card and lane
tails keep receiving bounded redacted lines on that same poll. They append in
time order, follow the newest line, and pause automatic scrolling when you scroll
up. The new-line button resumes following. Last-line age names its source as the
last observed event; it is not independent proof of agent progress. Sections
pause briefly after interaction and while focused or selected. A visible banner
reports refresh failures while retaining the last data. All assets are embedded,
offline; data and actions stay authenticated. CSP permits only same-origin scripts/styles, with no
inline code. All report and note text is inserted as text. Output stays redacted;
raw tool results, session IDs and stderr are not dashboard data.

Send a message to an ended session, queue one message for delivery after a
running run ends, or choose “Stop and send”. The pending message is private and
durable. Freeze suppresses delivery. Duplicate queued messages are refused.
Cancel, Freeze, Unfreeze and owner-note entry are local owner actions.


## Narrow screens and ETA

At widths of 480px or less, per-agent history scrolls horizontally with
unsplit cells. Other wide tables scroll within their panel; the main History
view retains its stacked rows. Long paths in live tails wrap. ETA keeps its
short text, with provenance in its title and on tap. Tests use the offline
DOM and stylesheet with a 390px predicate; visual review belongs to the owner.
