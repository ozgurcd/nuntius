# Changelog

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
