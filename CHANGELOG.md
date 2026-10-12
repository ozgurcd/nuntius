# Changelog

## Unreleased

- Replace held-message receipt snapshots instead of accumulating old rows.
  Keep only reviews still being read or acted on, within the server's row and
  receipt ceilings, and explain when fewer latest receipts are shown.

- Add the internal native goal schema and strict decoder, with explicit work
  scope, engine/work-class separation, holds and bounded safe metadata. Creation
  grants no approval. No native storage, launch or public operation is enabled.

- Add identity-bound held-message review, explicit Send and Discard, durable
  receipt explanations and retained removed-lane Resume. Resume never replays
  messages. Unknown or timed-out results reread once without claiming delivery.
- Show Running, Available, overlapping Paused and outcome-based Ended totals
  from one operational snapshot. Keep missing-report attention independent of
  holds and clear held rows, private selectors and drafts on sign-out.

- Fail closed when fresh lane controls are unavailable, even at a lower
  fallback revision. Explain newer changes after stale save replies and read
  current state once without retrying the change. Say when a lane was already
  paused or resumed instead of claiming a new saved change.

- Add one guarded dashboard lane-control client with Pause lane, Pause after
  current and Resume lane, revision checks, whole-save deadlines, uncertainty
  rereads and sign-out ownership. Keep hold badges separate from run outcomes,
  block new task messages on paused lanes, and preserve focus, tail and Cancel.
- Explain Agents activity, ages, duration provenance and current/previous report
  association. Sort attention before working lanes while keeping readers in
  place, with selectable lane order and wrapping phone labels.

- Add guarded revision-checked HTTP lane pause/resume and identity-bound held
  send/discard, fixed plain errors, bounded requests and one shared dashboard
  mutation limit. Make operational GETs observe controls and ended runs without
  creating files, repairing intent or starting work.

- Use safe process observation in every lane-control snapshot, including MCP
  pause/resume results and refusal fallbacks. Ended runs no longer appear to be
  draining; surviving and unverified processes keep their running blocker.

- Observe ended processes in CLI status, agents and lane lists without changing
  stored data or starting recovery. Restore missing-report attention and show
  the local date and time zone for lane holds.

- Add judge MCP lane pause/resume with required revision checks, strict closed
  arguments and structured JSON plus identical text. Explain paused refusals
  and preserve independent status/safety tools when lane controls are corrupt.
  Require host Ask for both controls; avoid duplicate manager mutation audits.

- Add CLI lane pause/resume/list and explicit held-message review/send/discard
  with durable content receipts. Keep read-only CLI status/agents/list free of
  file creation, intent repair and child launch. Expose versioned safe holds
  separately from run state, failed outcomes, report presence and attention badges.

- Reports & runs shows the report's summary, status, owner questions, commits
  and next steps before its full redacted text. Missing or incomplete answers
  stay visible, and a normal process exit does not prove the work is complete.
- Open agent tails follow new output. Scrolling up pauses automatic scrolling
  and shows a button to jump to new lines. The same refresh rate and private
  output protection apply.
- Bound dashboard, Queue and Settings GETs to ten seconds through body decoding;
  explicitly settle timeouts, abort obsolete reads and discard superseded replies.
  Refresh immediately on visible/focus/pageshow, preserve Settings drafts and
  show last-success age after failed reads. Never retry mutation requests.

## v0.3.4 — 2026-10-11

- Keep recognizable private blocks in agent output out of cached and served
  activity, including split, nested, displaced and incomplete markers at the
  end of a file. Apply the same protection to terminal-message fallback.
- Reduce busy dashboard CPU by reusing bounded parsed event windows and avoiding
  repeated redaction scans; cancel and freeze still send stop signals immediately.
- Reset the signed-out header to a neutral badge with no retained freeze colour,
  overview counts or refresh timestamps, including while reload or replies wait.
- Clear the freeze badge tooltip/accessibility label and navigation counts
  synchronously on sign-out, expiry, cross-tab sign-out and server rejection,
  including before reload completes and during pending actions.
- Wait briefly for Settings GET lock contention with one cancellable deadline;
  show specific read failures and retry busy reads within a limit while preserving
  drafts. Saves still refuse busy locks without writing or automatic retries.
- Use metadata-only idle maintenance and reuse its shared history snapshot,
  while keeping recovery checks and full detail for explicit status reads.
- Return known background dashboard startup causes and the full web.log path;
  distinguish Tailscale connection/CLI/status failures, busy local ports,
  conflicting Serve mappings and Funnel. Persist only allowlisted failure codes,
  not captured output, and clear stale startup results before a new launch.
- Record the running dashboard version and show it in web status/status --long;
  warn when it differs from the CLI version or an older record lacks a version.
- Show one shared signed-out panel on every dashboard tab, including Settings;
  hide private forms and connection controls until sign-in. Keep immediate
  sign-out, expiry, revocation and late-response guards covered offline.
- Supervise test dashboards with compiled offline fixtures, unconditional
  cleanup and explicit child reaping, including partial startup failures.

## v0.3.3 — 2026-10-11

- Stabilize Settings conflict tests by stopping unused fixture status workers;
  preserve stale-revision and lock-refusal assertions, with additional content
  identity and locked stale-request controls. Runtime behavior is unchanged.

- Keep optional Queue file failures out of configuration reloads, so operational
  dashboard data, dispatch and reply continue while Queue reports unavailable.

- Add a read-only Queue tab for coordinator-published portable v1 snapshots,
  ordered goals, independent owner/prerequisite holds, AND filters and mobile cards.
- Add guarded GET /queue, a local-only queue_file override/disable, bounded
  schema validation, descriptor-rooted no-follow reads and allowlisted redaction.
- Preserve filters/focus/details during queue polls, warn on stale/failed reads,
  and clear private rows on sign-out/expiry with offline regression proofs.

- Add the approved dashboard Settings tab with existing pool-size and iMessage
  preference editing, current/draft review, atomic Save, Cancel and phone layout.
- Share CLI validation/writing, guard Settings with exact Host/token/Origin,
  bound framing/rates/revisions and record durable safe audit intent/outcomes.
  Keep templates, paths, recipients, execution and startup options local-only.
- Replace menu-bar presets and file reveal with credential-free Settings…;
  keep minimal counts and the local helper start-at-login toggle.
- Cover conflicts, partial-save refusal, failure recovery, draft retention and
  sign-out during pending settings saves with offline fixtures.

- Hide read-only dashboard audit rows by default, with a browser-remembered
  Show reads checkbox, matching-entry count and combined text/lane/action filters.
- Derive read classification from existing MCP tool annotations; keep unknown
  actions visible and preserve the complete stored audit log and request guards.
- Replace empty audit lane/goal/brief cells with all lanes and muted n/a.
  Cover classification, stored-log integrity and filtering with offline tests.

## v0.3.2 — 2026-10-10

- Protect test-fixture manager configuration updates with the existing mutex;
  preserve launch-identity assertions and production locking.

- Keep dashboard Sign out enabled while an action is pending. Cover immediate
  local credential removal and rejection of late success, refusal, revocation
  and network-failure responses with disabled-aware offline browser clicks.
- Shorten the tail-contract test's temporary working directory so the complete
  cwd assertion fits the unchanged 120-rune action-summary cap.

- Clean up an exact owned HTTPS mapping after a partially failed child Serve
  create, even with --tailnet-keep; preserve foreign or uncertain mappings and
  report cleanup failures alongside setup failure.
- Add offline regressions for recorded loopback state, graceful signaling,
  last-moment process identity, final tailnet/Serve readback, redirect refusal,
  no-store readiness and exact private state mode. Clarify that judge-only
  ensure relies on registration and OS authority rather than caller identity.

- Remember dashboard sign-in per browser origin for seven days without renewal
  on reload, so phone bookmarks survive tab discard. Keep header-only API auth,
  exact Host/Origin checks and per-start grant revocation.
- Add explicit browser sign-out, cross-tab clearing, expiry checks, 403 cleanup
  and stale-response guards. Document the longer trusted-profile exposure and
  cover persistence/refusal with offline fake-clock browser predicates.

- Add judge-only stdio `nuntius_web_ensure` with a closed empty schema,
  state-changing/idempotent annotations and bounded, allowlisted state results.
- Serialize dashboard recovery with the existing lifecycle lock; preserve
  healthy process/grant, repair missing Serve, or start/replace one verified
  dashboard with required tailnet policy and graceful-only shutdown.
- Bound fixed-argv Tailscale preflight/output, validate unambiguous Serve JSON,
  refuse foreign 443/Funnel, read back writes and recheck exact ownership before
  cleanup. Record identity-bound mapping repairs without grant rotation.
- Probe authenticated readiness with header-only grants, exact Host, no proxy,
  redirect or body logging. Send new grants only to enabled configured Messages
  recipients on stdin, never in MCP results, audit, argv or environment.
- Add fake-only recovery, refusal, concurrency, cancellation, transport, private
  mode, request-guard and leakage tests; document limits and owner phone pilot.

- Classify complete OpenCode terminal turns without a current report as
  report_delivery_failed / ended_no_report; suppress automatic crash recovery
  while preserving unknown exit attribution and the existing recovery limit.
- Observe complete bounded streams rather than tails; reject partial, damaged,
  unbalanced or over-budget evidence. Keep live processes supervised.
- Show report-missing attention in status, history and dashboard; retain bounded,
  redacted final text. Use a neutral unknown-exit recovery note.
- Add offline terminal/restart, crash-chain, report freshness, redaction,
  corrupted-stream, inactivity/runtime and dashboard indicator regressions.

## v0.3.1 — 2026-10-10

- Rewrite newcomer README and preserve measured references in docs with
  compatibility anchors; update configuration, CLI and dashboard details.
- Add --version/-v aliases, structured ETA provenance and hover/tap explanation.
- Mark template fallback for legacy run identity as configured.
- Contain narrow history tables and long live-tail paths; verify offline at 390px.
- Add safe set --json snapshot, atomic notification on/off preserving recipient,
  and confirmed menu-bar settings through existing CLI validation.
- Include the post-v0.3.0 resumed missing-report attention fix.

## v0.3.0 — 2026-10-10

- Add a separate fyne.io/systray module: Cocoa template Relay icon, Linux
  StatusNotifierItem, fake-testable controller, three-second status refresh,
  lane rows, attention state and dashboard/phone/QR/lifecycle actions.
- Add a 0600 Unix socket and private browser/QR presentations. Keep sign-in
  credentials out of helper responses, process argv, environment and logs;
  pass iMessage data through stdin instead of argv.
- Start the installed helper with background dashboards, add --no-menubar,
  close it on dashboard stop, and offer explicit helper-only macOS login startup.
- Add accessible lane-tab state dots and offline mapping tests.
- Build helpers on macOS/Ubuntu CI; include them, app bundles and notices in
  release archives and the cask. Preserve the main CGO-disabled build.

## v0.2.11 — 2026-10-10

- Resolve Claude/Codex model flags and Claude effort flags in split and equals
  forms. Record launch model/effort per run; capture Codex structured startup
  effort when reported, otherwise use `default`. Never read agent config files.
  Show identity in CLI agents/status/history, MCP status/history and dashboard.
- Add readable `audit [--since 1h] [--json]` and structured dashboard audit rows,
  newest first, with local times, action/lane filters, redacted refusal reasons,
  short brief digests and optional redacted raw records.
- Add All and per-agent dashboard tabs with hash routing, live newest-first tails,
  lane history/report links, lane owner notes and pending signals. Reconcile pool
  changes at runtime; keep the existing main dashboard views.
- Cover flag variants/defaults, recorded launch identity after configuration
  changes, structured effort persistence, audit safety/order/filtering, and
  dashboard routes/runtime pool changes with offline fake-agent and Node tests.

- Add user-facing release notes for the existing versions. Require matching,
  nonempty release-note and changelog sections in release checks and CI, with
  offline refusal fixtures. Use the selected release-note section as the GitHub
  release body and include the release notes in downloadable archives.

## v0.2.10 — 2026-10-10

- Add `web start`, `stop`, `status` and `link [--notify]`: a transient per-user
  macOS launchd job or Linux detached session frees the terminal and survives
  terminal closure. Wait for loopback readiness, prevent duplicate starts,
  support `--replace`, cleanly release owned Serve mappings and remove access
  files on stop; foreground `web` remains compatible.
- Store background link access only in an owner-only 0600 file; keep credentials
  out of daemon logs and status. Show the same dashboard line in `status --long`.
- Ignore reports from earlier runs of a redispatched goal for report presence,
  stuck detection, recovery and dashboard history.
- Let `set` list all writable pool sizes and their current values/sources,
  or read one key; identify read-only config without exposing template secrets.
- Render terminal QR codes with one-column half-blocks, qr.L and a two-module
  quiet zone; suppress QR on known narrow terminals. Decode rendered text in
  offline pure-Go tests.
- Embed the owner's CC0 Relay logo and theme-aware favicon in the dashboard,
  and show light/dark marks in README. Document background phone access.

## v0.2.9 — 2026-10-10

- Add `web --tailnet`: find the Tailscale CLI, require a running backend with a
  MagicDNS name in the HTTPS certificate domains, use that name as the allow
  host, and stop with one plain sentence naming what to turn on;
  `--tailnet-optional` falls back to the loopback dashboard.
- Set up Tailscale Serve itself: reuse an `https:443` mapping that already
  proxies to the dashboard port, otherwise create one and confirm it; refuse
  with the exact command when 443 serves anything else or Funnel is on; remove
  only a mapping created this run on exit unless `--tailnet-keep` is given.
- Send the HTTPS sign-in link by iMessage to the owner's own handle with
  `--notify-imessage` or the `notify_imessage` config key, through a fixed
  AppleScript that takes handle and text as arguments; a failed send warns and
  the dashboard keeps running; the link never enters a log or state file.
- Print one credential-free summary line (tailnet host, serve reused/created,
  iMessage sent/failed) at start, and show whether a tailnet dashboard is active
  in `status --long`.
- Document the one-command phone path, the Messages permission prompt, what
  nuntius changes in Tailscale and undoes, the Funnel refusal and the manual
  fallback.

## v0.2.8 — 2026-10-10

- Colour dashboard badges from a server `badge` tone (green, amber, red) in
  status and history JSON instead of matching label text; a resumed run that
  is running or finished normally is green, and a failed, cancelled or
  timed-out resumed run keeps that outcome's colour.
- Read finished resumed runs as outcomes — `Ended · resumed 1/2`,
  `Failed · resumed 1/2`, `Cancelled · resumed 1/2`, `Timed out · resumed 1/2` —
  in status, status --long, history, MCP status/history and the dashboard.
- Document the dashboard colours: green = working or finished normally,
  amber = needs a look, red = failed.

## v0.2.7 — 2026-10-10

- Inherit the dispatching process's startup umask in transient launchd jobs,
  with fallback 022, instead of forcing 077; retain private run-file modes.
- Add a single exact `web --allow-host` tailnet DNS opt-in while keeping
  loopback binding, exact Host/HTTPS POST Origin guards and header-only API auth.
- Print private tailnet and loopback sign-in links plus a terminal QR code
  using pure Go rsc.io/qr v0.2.0; never trust forwarded headers for access.
- Document phone access through Tailscale Serve, its security limits and awake
  Mac requirement; no Tailscale settings or home-screen/PWA changes.
- Label active recovery runs `Running · resumed 1/2` across status, agents and
  dashboard; prove behaviour with fake launchctl and offline request/DOM tests.

## v0.2.6 — 2026-10-10

- Add OpenCode pools and legacy lanes, explicit owner-selected provider/model,
  as_is starts, conversion/resize and model visibility in CLI, MCP and dashboard.
- Start JSON runs with intact brief stdin and resume captured OpenCode sessions;
  retain transient Darwin launchd, identity checks and detached fallback.
- Normalize OpenCode text, tool inputs, step events, errors and timestamps through
  shared redacted action summaries; never return tool results.
- Cover cancel, timeout, freeze, session/fresh crash recovery, job cleanup and
  history chains with offline fake OpenCode tests; never call a real model.
- Document guard/permission installation pointers and explicit model choice;
  fixtures follow upstream schema and remain unverified by live model output.

## v0.2.5 — 2026-10-10

- Share redacted one-line action summaries across watch, MCP and dashboard tails:
  actual commands, edit/read paths, searches, MCP arguments and task descriptions.
- Add status --long and running-lane current_action with action age and matched
  unfinished-command duration; mark untimestamped observation ages approximate.
- Replace opaque history links with readable continuation/recovery relations;
  group recorded goal chains and indent descendants in human/MCP/web history.
- Use consistent eight-character run IDs in human, MCP and web views; retain
  full IDs and existing numeric/boolean/null types in CLI --json output.
- Add sanitized real Claude/Codex action-event fixtures and offline parser,
  CLI/MCP/web, timing, history and dashboard tests; exclude tool output/results.
- Keep v0.2.4 transient launchd, detached fallback and recovery behaviour unchanged.

## v0.2.4 — 2026-10-10

- Start macOS agents in transient, one-shot GUI launchd jobs outside the app's
  job; retain Linux double-fork starts and install no service or login item.
- Keep exact argv, cwd and environment with file-backed standard streams and
  an exec-only bootstrap; ferry environment in memory through a private pipe.
- Remove only recorded per-run jobs on completion, cancel and timeout; retry
  stale cleanup on recovery and refuse unverifiable live process identities.
- Fall back to detached start on launchctl failure, with fixed events/status
  warnings; ambiguous partial starts remain recorded without duplicate agents.
- Add offline injectable-launchctl tests for lifecycle, recovery, label and PID
  safety, fallback, pending launches, restart/freeze and environment preservation.
- App-quit survival and macOS one-shot cleanup require the owner's live pilot;
  sandbox inspection cannot establish the pilot's exact process-kill mechanism.

## v0.2.3 — 2026-10-10

- Launch agents through short-lived double-fork stages, with their own session,
  file-backed standard streams, OS parent and no persistent helper or service.
- Record every goal before launch; recover on MCP startup or `nuntius recover`.
  Resume the recorded session or resend the same brief with a restart note.
- Default auto_resume on, bound each chain to two recovery attempts, preserve
  busy-lane waiting and exclude cancelled/timed-out runs and frozen dispatch.
- Show interrupted, resumed counts and waiting in CLI/MCP/web and link history.
- Derive detached outcomes from reports; keep uncollectable exit codes unknown.
- Fsync directories after atomic private writes and add run integrity checks;
  ignore torn records and signals without hiding intact goals.
- Prove parent loss, MCP kill, executable replacement, session/fresh recovery,
  exclusions, waiting and corruption using offline fake agents only.

## v0.2.2 — 2026-10-09

- Add bounded `history` CLI and read-only `nuntius_history` MCP filters for
  finished runs, including missing reports, cancellation and timeout.
- Distinguish timed-out and cancelled runs in status, agents and web views;
  preserve existing JSON states with an additive display_state.
- Mark runs `stuck?` after five minutes beyond report modification or fifteen
  minutes without events; emit one durable stuck signal per run, without cancel.
- Use short Enabled/Frozen phone labels with full accessible dispatch text,
  retain tab-scoped sign-in across reload and render focused hash panels.
- Redesign local oversight as a responsive light/dark operations dashboard
  with summary counts, state badges, human times, ETA and bounded activity.
- Preserve focused inputs, expanded history, tails and audit while polling;
  add history filtering, refresh freshness and a visible data-error banner.
- Confirm cancellation in-page and retain all existing owner actions.
- Keep controls and counts in a sticky header with accessible, hash-selected
  tabs; compact agent cards link to the message composer. Stack phone history
  rows, align desktop cells and show idle placeholders in normal muted type.
- Embed offline HTML/CSS/JavaScript, guard every asset request and tighten
  script/style CSP to same-origin assets without unsafe-inline.
- Add guarded-asset and data-shape tests plus a realistic redacted dashboard
  fixture; preserve owner installation and existing CLI/MCP machine state values.

## v0.2.1 — 2026-10-09

- Show aligned human status by default, with freeze/data header, elapsed,
  bounded activity kinds, event age, report presence and median same-size ETA.
- Preserve status JSON with --json and keep MCP output JSON.
- Resolve config/data paths from flags, environment and macOS/Linux defaults;
  add paths provenance, empty-store warnings and app environment guidance.
- Skip notifications outside macOS and enforce Darwin/Linux-only releases.
- Create atomic empty end signals for all terminal outcomes; add MCP listing
  and confined acknowledgements with offline tests.

## v0.2.0 — 2026-10-09

- Detach each agent run into its own session and recover supervision by PID
  and kernel start time after a server restart. Refuse unverifiable signals.
- Add durable freeze, owner notes and acknowledgements, and event-age status.
- Add the loopback-only oversight dashboard with per-start authentication,
  Host/Origin checks, POST-only actions, bounded redacted views and queued replies.
- Add owner CLI pool sizes, atomic private config conversion and agents listing.
- Put Codex exec options before resume; enforce the real scope in the offline fake.
- Document mobile oversight, owner tool approvals and pool conversion.

## v0.1.2 — 2026-10-09

- Publish macOS and Linux archives for amd64 and arm64 with release checksums.
- Publish the Homebrew cask to ozgurcd/homebrew-tap after checking its credential.
- Use Homebrew for installation; never install nuntius into ~/go/bin.
- Keep the v0.1.1 runtime behavior unchanged.

## v0.1.1 — 2026-10-09 (not tagged)

- Permit Claude permission modes auto, acceptEdits, plan and manual.
- Permit Codex --skip-git-repo-check and read-only/workspace-write sandbox modes.
- Keep all other argument restrictions, including agent-specific flag checks.
- Test accepted forms, forbidden modes, unknown/missing values and wrong agents.

## v0.1.0 — 2026-10-09 (not tagged)

- Add a local MCP server for the judge to start, follow, resume and cancel
  configured Claude and Codex runs.
- Store private run records and an append-only audit log.
- Refuse busy lanes and permission or hook overrides.
- Redact and limit returned agent text and reports.
- Continue a successful Claude acknowledgement once, in the same session, when
  the complete event stream shows no work. Link both run records.
- Add owner commands and offline fake-agent tests.
