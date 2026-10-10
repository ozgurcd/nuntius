# Portable queue format v1 (Unreleased)

Any coordinator can publish a complete JSON snapshot. Nuntius displays the
coordinator's plan; it does not approve, plan, resolve dependencies, execute
briefs or dispatch. Readiness does not prove an idle agent, a cleared freeze,
an acquired claim or authority to start work. Started and finished work belongs
in Agents and History, and the coordinator removes it from the next snapshot.

## Location and local configuration

Omit `queue_file` to read `queue.json` in the resolved data directory, selected
by `--store`, `NUNTIUS_DATA_DIR` and OS defaults. On macOS the default is
`~/Library/Application Support/nuntius`; on Linux it is `$XDG_DATA_HOME/nuntius`
or `~/.local/share/nuntius`. The configuration directory is never the default.
Set `"queue_file": ""` locally to disable Queue, or use an absolute, clean
local file path to override it. Paths with links or special files are refused;
an explicit missing path never falls back to the default. The local owner
sets the path and checks the configuration. Settings shows only its mode
(default/configured/disabled), with no browser editor or absolute queue path.

Nuntius opens the snapshot read-only. It never creates, repairs, truncates,
locks or deletes it. One coordinator owns each destination. For example, the
owner can point `queue_file` at `/srv/coordinator/queue.json`, then have the
coordinator publish there. A coordinator adapter should require its output
path as an argument and must not edit owner configuration.

## Document and goal schema

Use UTF-8 JSON: one object followed by EOF, no BOM, comments, duplicate keys
(including ignored keys), or trailing data. Known keys have the exact types
below; null is not omission. Limits count UTF-8 bytes. Maximum document size
is 1,048,576 bytes; maximum container nesting depth is 8, including ignored
fields. Unknown fields at every object level are ignored only for version 1
and are never returned or used for control decisions. Future versions fail.

| Document field | Required type and rule |
| --- | --- |
| version | Integer exactly 1. |
| updated | UTC RFC3339 with seconds: `YYYY-MM-DDTHH:MM:SSZ`, publication time. |
| goals | Array of 0–500 goal objects, the complete pending queue. |

| Goal field | Type and rule |
| --- | --- |
| id | Required unique case-sensitive string, 1–64 ASCII bytes, `^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`. A display identifier, not dispatch authority. |
| title | Required plain string, 1–240 bytes after trimming. |
| size | Required string S, M or L; coordinator-defined effort. |
| lane_kinds | Required array of 1–8 unique lowercase slugs, each `^[a-z][a-z0-9-]{0,31}$`. Unknown kinds remain labelled. |
| lane | Optional exact lane preference/restriction, using the ID grammar. It need not currently exist. |
| order | Required integer 0–2147483647; ascending, with ASCII ID ties. Gaps allowed. Filtering never renumbers it. |
| depends_on | Required array of 0–32 unique IDs; no self-dependency. Missing IDs are external/unresolved, never assumed complete. |
| needs_owner | Required boolean, independent of state. |
| owner_reason | Required nonblank plain string of at most 512 bytes when needs_owner=true; absent when false. |
| state | Required string planned, blocked, ready or parked. |
| reason | Optional plain string of at most 512 bytes; required nonblank for blocked and parked. |
| created, updated | Required UTC timestamps as above; created <= updated <= document.updated. |
| brief_sha256 | Optional string of 64 lowercase hexadecimal characters, digest of exact brief bytes. Provenance is not approval. |
| brief_link | Optional HTTPS URL of at most 2048 bytes; no userinfo, query, fragment or control characters. See link policy below. |

All display strings are plain text, not Markdown or HTML. Control characters,
including newlines, NUL and bidi override/isolate controls, fail validation.
One invalid goal invalidates the whole snapshot; Nuntius never silently drops
malformed goals and implies completeness. In-snapshot dependency cycles fail.

`planned` means still being prepared; `blocked` means the coordinator cannot
proceed, with a nonblank reason; `ready` means prepared with no owner hold or
dependencies; `parked` means explicitly deferred, with a nonblank reason.
Owner holds and prerequisites remain separate, visible concepts.

```json
{
  "version": 1,
  "updated": "2026-10-10T22:32:00Z",
  "goals": [{
    "id": "EXAMPLE-DOCS",
    "title": "Review the documentation",
    "size": "S",
    "lane_kinds": ["codex", "claude"],
    "order": 4,
    "depends_on": ["EXAMPLE-REVIEW"],
    "needs_owner": true,
    "owner_reason": "Approve the public scope.",
    "state": "blocked",
    "reason": "The review and owner decision are pending.",
    "created": "2026-10-10T22:28:00Z",
    "updated": "2026-10-10T22:28:00Z"
  }]
}
```

## Atomic producer protocol

Use an existing private directory (0700) owned by the coordinator/owner.
Validate the entire candidate before publishing. Exclusively create an
unpredictable same-directory temporary regular file with mode 0600 and no
link following. Write all bytes, flush and fsync, close, then atomically rename
over the destination and fsync its directory where supported. Never truncate
the destination in place or follow its symlink. Readers see complete old or
new snapshots. Publish `goals: []` to clear the queue.

Before rename, failure preserves the previous snapshot and removes only this
writer's temporary file. After rename, a directory-fsync failure means durability
is uncertain; do not claim rollback. Serialize coordinator events and exporters
before collection as well as publication. Nontransactional source files must
be checked for changes during collection; retry once or fail without publication.
Publication does not make a source ledger transactional. No cross-process CAS
is promised; serialization belongs to the coordinator.

## API, read limits and links

Authenticated GET `/queue` uses the existing exact Host/port, header-token,
method and POST Origin guards. It is absent from the public-shell allowlist.
GET needs no Origin. Nonempty queries/selectors fail; the browser cannot choose
a file, path or URL. POST/PUT/DELETE/PATCH/OPTIONS cannot read or mutate Queue.
No CORS, cache, filesystem traversal API, brief fetch, watcher, scheduler or
MCP tool is added. Existing no-store, nosniff and same-origin CSP apply.

The reader uses descriptor-rooted no-follow operations for every path component
and the leaf, nonblocking opens and a descriptor regular-file check. It refuses
symlinks, FIFOs, sockets, directories and devices. It reads at most 1,048,577
bytes and rejects oversize data before parsing. Detected changes during a read
fail safely. Read errors affect Queue only, never the operational data view.

The allowlisted, redacted response is `{status, version?, updated?, goals, reason?}`,
with goals always an array. `ok`, `not_configured` (absent default) and `disabled`
are HTTP 200. `missing` (absent override), `invalid`, and `unavailable` are HTTP
503 with fixed reason codes `file_missing`, `invalid_snapshot` and `read_unavailable`.
No original JSON, unknown field, source path or parser/OS error is returned.

Metadata is sensitive to every dashboard grant holder. Producers must omit
passwords, secrets, grants, cookies, sessions, prompts, execution details and
token/code/state URLs, including in ignored fields. Redaction is defence in
depth and cannot identify every secret.

Links are optional and untrusted. Reject non-HTTPS, localhost, `.localhost`,
`.local` and literal/encoded IP hosts, relative/data/javascript/file URLs,
credentials, queries and fragments. The UI validates again and opens a link
only on deliberate user action with noreferrer/noopener. Nuntius never fetches,
previews, proxies or downloads briefs. No local brief route exists.

See [dashboard behavior](dashboard.md#queue-unreleased) and [security](security.md).
