# Changelog

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
