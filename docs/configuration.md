## Owner configuration

Create `~/.config/nuntius/lanes.json`. Use absolute executable and workspace
paths from your machine. The following paths are placeholders, not defaults:

```json
{
  "pools": {
    "claude":{"size":1,"template":{"workdir":"/absolute/workspace","binary":"/absolute/bin/claude","extra_args":[],"start_mode":"goal_then_continue","continue_text":"continue"}},
    "codex":{"size":2,"template":{"workdir":"/absolute/workspace","binary":"/absolute/bin/codex","extra_args":[],"start_mode":"as_is"}}
  },
  "max_runtime":"8h",
  "reports_dir":"/absolute/workspace/reports",
  "notifications":true
}
```

`notifications` is optional and defaults to true. Lane `start_mode` defaults
to `goal_then_continue` for Claude and `as_is` for Codex. `continue_text`
defaults to `continue`. Set Claude to `as_is` to disable automatic continuation.
Codex and OpenCode support `as_is` only. The text and start mode cannot be supplied by MCP.
`auto_resume` is optional and defaults to true. Set it to false to keep
interrupted goals visible without automatically starting another agent.
`notify_imessage` is optional: your own iMessage handle (an email address or an
E.164 phone number such as `+15551234567`) that `nuntius web --tailnet` sends
the phone sign-in link to. `--notify-imessage` on the command line overrides it.
An invalid handle is refused at load. See "Phone access over Tailscale".

Each kind has one template and a size from 0 to 8. Agents are named
`claude-1` through `claude-N`, `codex-1` through `codex-N`, and `opencode-1` through `opencode-N`. A disabled
pool retains its template. Templates are validated even when their size is zero.
No MCP tool changes pool sizes or templates. The CLI rewrites configuration
atomically with mode 0600; shrinking below a busy agent is refused.

Legacy `lanes` files still load. When all lanes are idle, convert them:

```sh
nuntius config convert
nuntius set claude-agent-pool 1
nuntius set codex-agent-pool 2
nuntius agents
nuntius agents --json
```

Conversion maps the Claude lane `identuum` to `claude-1`. Agents of each
kind are numbered in their original lane order. It preserves recorded sessions
under their converted names. Differing templates within one kind, more than
eight agents of one kind, or busy lanes refuse conversion without guessing
which settings to keep. Reconcile templates in the owner file first.
Existing servers reload configuration on status checks and before dispatch.

Unknown fields, duplicate lane names,
relative paths, non-executable binaries, invalid working directories, empty
lane sets, more than 32 lanes, and invalid durations are refused. `max_runtime`
must be positive and no longer than seven days. Configuration is limited to
64 KiB. No setting is read from agent configuration files by nuntius.

`extra_args` supports `--model`/`-m` for Claude and Codex, and `--effort` and
`--max-budget-usd` for Claude. Claude also permits `--permission-mode` with
exactly `auto`, `acceptEdits`, `plan` or `manual`. Codex permits
`--skip-git-repo-check` without a value and `--sandbox` with exactly `read-only`
or `workspace-write`. Value-taking options support split and `--name=value`
forms. These options are agent-specific. Other options
are refused, including permission/sandbox bypasses, hook or settings overrides,
working-directory changes, session selection and output-format overrides.
This also closes indirect bypasses through Codex `-c` and Claude `--settings`.
The child inherits the server environment unchanged. The owner must start
Desktop with the intended normal environment and agent settings.

## opencode lanes

Install [OpenCode](https://opencode.ai/docs/) separately and log in using its
own tools. Login stays in OpenCode's own configuration; nuntius never reads or
copies it. Add an owner-chosen provider/model to the pool template:

```json
{
  "pools": {
    "opencode": {
      "size": 1,
      "template": {
        "workdir": "/absolute/workspace/repository",
        "binary": "/absolute/bin/opencode",
        "model": "opencode/minimax-m2.7",
        "extra_args": [],
        "start_mode": "as_is"
      }
    }
  },
  "max_runtime": "8h",
  "reports_dir": "/absolute/workspace/reports",
  "notifications": true
}
```

The model is an example, not a free-model claim or a default. Choose the exact
OpenCode Zen model ID and check its current cost with OpenCode. Nuntius requires
an explicit `provider/model` and never chooses, upgrades or falls back to another
model. Keep it in the template's `model` field; `--model` in `extra_args` is
refused to prevent conflicting selections. Optional `--agent` and `--variant`
accept split or equals forms. No MCP request can select a model or change flags.

For Identuum, follow the owner-install steps in
`wiki/tools/opencode/README.md` for the guard plugin and
`wiki/tools/opencode/opencode.permission.json` template. OPENCODE-GUARD-1 owns
those files. Install the plugin in the documented plural `plugins/` directory
and merge only the permission member, preserving the owner's other settings.
Review effective agent overrides and launch inside the configured repository
so its parent `AGENTS.md` rules apply. Nuntius installs or edits none of these.
The guard is a tool hook, not an OS sandbox; see its guide for containment limits.

Run `nuntius config check`, then `nuntius set opencode-agent-pool 1` or
`nuntius agents` to inspect the pool. The template must already exist. Legacy
OpenCode lanes also support `config convert`; different models within one kind
refuse conversion. Status, agents, history, MCP and the dashboard show kind and
model and effort; historical runs retain their launch values after configuration changes.

Starts use `opencode run --format json --model MODEL [extra_args]`, with the
brief supplied intact on stdin. Resume adds `--session ID`; missing IDs use the
same brief with the existing interruption note during crash recovery. Darwin
uses the same transient launchd job and identity checks as other agents. Cancel,
timeout, freeze and the bounded recovery chain follow the same rules.

Verified CLI help: OpenCode 1.18.35. Its `--auto` flag automatically approves
permissions not denied, but nuntius refuses permission overrides, including
`--auto`, as well as `--pure`, attachments, remote-server selection, interactive
mode and output/session overrides. Configure permitted mechanical tools in
OpenCode's normal permissions instead. Nuntius inherits the environment without
injecting `OPENCODE_PERMISSION` or changing settings.

Event fixtures are derived from
[upstream run source](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/cli/cmd/run.ts),
not a live model capture. They remain unverified against real 1.18.35 model
output. An isolated free-only Step 5 attempt returned an error event and no
successful model text; no paid model or owner configuration was supplied.
The documented JSON stream supplies `sessionID`, millisecond timestamps,
text, step events, errors and nested tool state/input. Summaries exclude results.
Current upstream emits tool parts at completion/error only, so running-command
duration is unavailable without a start event. Native MCP names use
`server_tool`; display splits at the first underscore, and server names containing
underscores can be ambiguous. Unknown event types retain a bounded first string
argument, excluding identity/result metadata. Raw private streams stay on disk.


## Notification toggle

Set your own recipient in notify_imessage in the owner file. Then use
`nuntius set notify-imessage off` or `on` without losing that recipient.
The optional notify_imessage_enabled boolean records this choice; omitted means
enabled when a recipient exists. Enabling with no recipient is refused.
`set --json` exposes display settings without recipient or template arguments.
The dashboard Settings page uses the same Manager policy and private writer as
these CLI setters. It can change existing pool sizes and this boolean only.
Templates, recipients, paths and execution options remain local-only.

Lane holds are operational controls, not configuration settings. `lane pause`,
`lane resume`, `lane list` and held-message commands leave the selected lane
configuration byte for byte unchanged. Their private state belongs to the
selected data directory in `lane-control.json`, with a separate durable intent
for a save in progress. Do not edit these records by hand.
Shrinking or disabling a pool does not erase a saved name or hold. Re-adding
that exact name, including with a different agent kind, retains the hold.
`lane list` shows removed names as unconfigured and unavailable for dispatch.
You can resume their retained hold without growing the pool or recreating a
template. Resume starts no work and leaves a global freeze intact.
Read-only CLI list/status/agents never create the data directory or reconcile
a save in progress; they report unreadable controls as unavailable.

`queue_file` (since v0.3.3) is local-only: omit it for `queue.json` under the
resolved data directory, use an empty string to disable Queue, or supply a
clean absolute regular-file path without symbolic links. An absent explicit
file never falls back. Settings reports only default/configured/disabled;
it cannot read back or edit the path. The coordinator publishes snapshots,
and Nuntius only reads them. See [queue-format.md](queue-format.md).

## Native brief seals (local, unpublished)

The internal brief compiler keeps the author's body and the owner's shared
standards as separate exact UTF-8 bytes. A trusted local caller supplies the
profile name and revision, an explicit separator, the standards marker, and
either an empty wrapper or `/goal` followed by a newline. It appends the
separator and standards once. It does not expand templates or execute text.
This component adds no owner configuration fields, local file reader, CLI,
MCP tool or browser operation. Later adapters must select text or registered
artifact keys; they must not turn this into an arbitrary path reader.

The first body line must start `GOAL: ID (size S)`, using the stored goal ID
and S, M or L size. Optional comma-separated header details may follow the
size inside the parentheses. A short description may follow the parentheses.
The header must remain one line. Nuntius does not infer identity or size from
other prose. Invalid UTF-8, a byte order mark anywhere, a `PLACEHOLDER` prefix,
a missing or repeated standards marker, and mismatched headers need review.
The final body, separator and standards together may use at most 128 KiB.
Nothing is truncated. The private encoded seal must also fit its existing
256 KiB storage limit, including JSON escaping.
Assembly refusals explain the cause and give safe assembled and dispatch
byte/code-point counts. Invalid UTF-8 makes the code-point count unavailable.
The refusal never prints the body, standards or supplied references.

The seal binds the goal spec and revision, profile, authority reference and
reviewed source observation together with body, standards, final and dispatch
SHA-256 digests. It records UTF-8 bytes and Unicode code points separately.
Dispatch measurements include the `/goal` wrapper when declared. These
measurements do not yet provide delivery: the next component owns the direct
12,000-byte limit, per-engine code-point cap and full-artifact pointer rules.
Native launching and automatic dispatch remain unavailable.

Approval needs the exact sealed revision and authority. It also compares a
freshly supplied source observation with the sealed observation. If they differ,
`preflight_review_required` explains that the source changed and needs review
before resealing and approval. Nuntius checks these references, not the truth
of repository facts or human consent. A reference is not authentication.

Changed body or standards bytes offer a newer source without editing the old
seal or approved bytes. Refresh explicitly creates a new goal revision, returns
it to drafted, and removes the old approval and exact automatic grant. Review
and approve that new revision separately. A failed refresh preserves the old
seal. Bookkeeping sealing and approval start no child and leave lane controls,
pause, Resume, held messages and legacy dispatch unchanged.

The writer and saved-history reader recompute the same complete seal and
approval predicates. Changing execution scope removes the old approval and
exact automatic grant, even if the old seal and decision history are unchanged.
Entering approved state needs a new reviewed decision with the matching seal,
authority and source observation. Notes and title or ordering edits keep existing
approval and history. Closed JSON keys must match after unescaping; case aliases,
duplicate decoded keys, null fields and unknown fields refuse approval.
Profile names/revisions/markers, authority and source observations must each
be one nonblank line of at most 128 UTF-8 bytes. Unicode line and paragraph
separators and U+0085 are rejected in these one-line fields.
