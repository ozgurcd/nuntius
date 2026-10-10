# Action event fixture provenance

Captured on 2026-10-10 from completed and started events in the owner's local
Claude stream-json and Codex exec --json run files, with read-only extraction.
The fixture retains observed envelope types, tool names, input field names,
change kinds and lifecycle statuses. Every command, path, query, description,
text and action identity is replaced with a fixed public placeholder.
Prompts, sessions, edit bodies, tool outputs, results and errors are omitted.
Codex MCP arguments retain the observed `code`/`title` object shape.
No private run path or run identity is retained.

Additional table-driven cases exercise documented tools not present in the
captured set, unknown types, multiline inputs, Unicode bounds, redaction and
timestamped events. Neither real agent is executed by these tests.
