# Changelog

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
