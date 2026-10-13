<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/brand/nuntius-logo-dark.svg">
  <img src="../assets/brand/nuntius-logo.svg" alt="Nuntius Relay logo" width="96" height="96">
</picture>

# nuntius

Nuntius v0.3.5 is a small local Go MCP server for a judge using Claude Desktop.
It starts and follows configured Claude Code, Codex and OpenCode agent pools on macOS or Linux.
It contains no LLM and does not call model providers. Dashboard readiness
uses loopback HTTP; agent programs and optional Tailscale/Messages integrations
use the network under their existing settings. The Relay mark is dedicated to
the public domain under [CC0 1.0](../assets/brand/README.md).

Register nuntius in Claude Desktop only. Do not register it in Claude Code or
Codex or OpenCode. Coding agents must not dispatch other agents. Configure the host to ask
the owner before every dispatch, reply, cancel and unfreeze. MCP annotations describe
side effects; they do not enforce a click or identify who called the server.

`nuntius set` lists each writable pool size and its current value/source;
`nuntius set codex-agent-pool` reads one. Missing pools show zero from the
default; configured pools and legacy lane counts cite the config file path.
Pool sizes and iMessage on/off are writable by `set`. Templates, lanes,
max_runtime, reports_dir, notifications, auto_resume, oversight_path and the
iMessage recipient are owner-edited config keys. Tailnet flags default to off and are CLI-only.
Queries do not print template arguments, secrets or tokens, and redact paths.

## Build and check

Use the Go version in `go.mod` (1.27.2), Staticcheck 2026.2.1 (0.8.1), and
govulncheck v1.8.0. CI gets Go and the checksum-verified Staticcheck build from
the pinned Lictor composite action used by Legattus. The pure Go
[rsc.io/qr v0.2.0](https://github.com/rsc/qr) library generates terminal QR codes,
with no transitive modules. Go has no standard-library QR encoder; using the
existing encoder avoids maintaining QR error correction here. Tests use the
pure-Go goqr decoder (test-only; LGPL-3.0) to read the rendered terminal QR back.
It is not linked into released binaries. Tests also need Node.js (CI pins 26.8.1) to run the dashboard
JavaScript in an offline fake DOM; no browser or npm packages are needed.
macOS and Linux are supported; Windows is not a target.

### Menu bar

`nuntius web start` starts the installed Relay menu-bar helper in a desktop
session. Use `--no-menubar` to skip it. The icon shows idle, the running lane
count, or attention for stuck/failed work, an end without a report, or a waiting
question. Hover for lane counts. Each menu row shows lane, goal, state and
elapsed time. Actions open the signed-in local dashboard, send the configured
iMessage phone link, show the phone QR, and start/stop the dashboard. Quit menu
bar leaves the dashboard running; `web stop` closes both. Dispatch/cancel
remain in the dashboard.

On macOS, opt in with `nuntius menubar --at-login on` and disable with
`nuntius menubar --at-login off`. The helper alone starts at your next login;
it does not start the dashboard. Installation enables no login items.
On Linux, KDE supports StatusNotifierItem, XFCE needs its StatusNotifier
plugin, and GNOME normally needs an AppIndicator extension. An unsupported
desktop exits without affecting the dashboard.

The main command remains pure Go. Native code lives in a separate helper
module and macOS app. Its ad-hoc signature is not Developer ID signing or
notarization; Homebrew removes quarantine for installed files, while manual
downloads need normal Gatekeeper approval. Relay assets are CC0. The helper
API is an owner-only Unix socket; no sign-in link enters helper argv,
environment or logs. Browser/QR pages are 0600 and removed after two minutes
or dashboard shutdown. Keep a displayed QR private.

Agents lane-tab dots use grey for idle, green for running, violet for finished,
amber for stuck or terminal report-delivery attention, and red for failed/interrupted work. Hover and screen-reader
labels explain the state. See [the design note](menubar-design.md).

```sh
make verify
```

The v0.3.5 source privacy repair adds `make redaction-proof` to that native gate. It runs
permanent regression/property checks and semantic mutation overlays, then five
coverage-guided fuzz campaigns, each for 60 seconds with two workers. The
campaign recipes generate disposable keys only after decoding nonsecret inputs.
Mutation failures must be semantic; compile failures never count as proof.
See [the redaction contract](redaction.md) for scope and retained limits.

Read [Release notes](../RELEASE_NOTES.md) for user-facing changes and upgrade
actions, and [CHANGELOG](../CHANGELOG.md) for technical detail. Contributors must
add a section to both files for each release. `make release-notes-test` checks
offline refusal fixtures; `make release-check` refuses a source version with
missing or empty sections. The tagged release workflow checks the exact tag
version and uses its release-note section as the GitHub release body.

Install the published release through the owner's Homebrew tap. Never install
nuntius into `~/go/bin`.

```sh
brew install --cask ozgurcd/tap/nuntius
nuntius version
```

The tests compile an offline Go fake. They never execute the real agents,
load agent settings, or make network calls. The separate govulncheck gate reads
the public vulnerability database.

Offline children own process groups and register their kernel birth identities
under a test-owned lease. Fixture cleanup and TestMain stop those groups before
removing records. Lease loss also stops a fake after abrupt timeout or test-process
termination. Independent native fault groups share the existing four-group bound;
their slot remains held through child cleanup. These protections apply only to
compiled test programs and do not change installed agent supervision.

The fake web launcher gives each separate server session its own lease-watching
wrapper. The server remains its own session leader; the wrapper registers and
watches its kernel identity, so it is stopped on owner loss even without a
fixture guard of its own. The interruption regression
checks an unguarded stand-in after INT, TERM, KILL and test timeout; its release
marker and bounded fallback allow cleanup even for a failing regression.
Independent admission, held-message, record and replay matrices use the same
four-slot bound, held through cleanup. End-state waits inspect the small run
record while a verified child is live, then call real Status for the final
observation; deadlines and assertions stay unchanged. Persistence refusals keep
fixed public text while tests can report a fixed operation label and numeric
errno, without recording OS error paths or private contents.
Prelaunch fixture saves also share Go's fork barrier until their lock descriptors
close. This excludes temporary descriptor inheritance by unrelated test launches;
it adds no retry and does not alter the production nonblocking store lock.

Coordination locks now explicitly unlock their open-file description before
closing it. CLOEXEC alone cannot release a description inherited by a forked
child before exec. Native readers/writers, admission, dispatch, legacy controls
and Settings retain the same lock modes and genuine contention refusals. The
regression keeps a duplicated descriptor open while checking both active-owner
refusal and release after owner Close; the fork barrier remains in place.

Nested test helpers reuse the suite's compiled fake and CLI with its offline
PATH, while keeping their own stores and lifetime leases. Independent process,
admission and dispatch matrices use the existing four-slot scheduler through
cleanup. Fault groups that already own separate stores remain parallel; explicit
lock release prevents cross-fixture inheritance rather than serializing them.
Lifetime helpers establish the exact child identity before m.Run arms the real
five-second testing timeout. Registration, parent readiness and teardown bounds
are unchanged. The parent finishes reading readiness before Wait can close its
stdout pipe, and transfers the identity through a channel for safe cleanup.


## Menu-bar Settings

Settings reads the current CLI display values even with the dashboard stopped.
Pool sizes and iMessage on/off require a pending change followed by Confirm;
Discard clears it. Start at login uses the existing menubar --at-login path
with the same confirmation. Open settings file reveals lanes.json in Finder
on macOS. Other displayed values and lane model/effort are read-only.
