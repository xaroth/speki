# query

Runs one read-only SQLite `SELECT` over the user's vaults, for questions search can't answer: documents by
metadata, links between documents, missing descriptions, stale runbooks. The views, examples and the
sandbox it runs in are described in [query.md](../query.md); this page is the tool's contract.

## Input

| Input | Type | Default |
|---|---|---|
| `sql` | one SQLite `SELECT`, `WITH` or `VALUES` statement | required |
| `vaults` | list of vault names, at most 10 | every readable vault, if that's 10 or fewer |

A user who can read more than 10 served vaults has to name the ones to query. Only served vaults count
toward the 10. The limit is SQLite's default number of attached databases; [`search`](search.md) has no such
limit.

## Output

```text
vault	path	updated
acme/kb	services/edge-proxy.md	2026-10-09T14:02:11Z
acme/kb	runbooks/proxy-restart.md	NULL
2 rows
Not queried, so this result says nothing about them: acme/new (still building its index; try again
shortly). Don't conclude something is undocumented, or write a document that may duplicate one there.
```

The text is tab-separated, header first, with `NULL` for nulls, then a line with the row count. Binary
values come back as text. When rows were cut, at 200 rows or at the size of one answer, the count line
adds ", truncated: add a LIMIT or narrow the query to see the rest". Skipped vaults follow on their own
line, as in `search`: readable vaults that weren't covered because speki can't serve them, when `vaults`
was left out. The text isn't fenced; each cell is put on one line and escaped instead
([overview.md](overview.md#results)).

## Limits

| Limit | Value | When reached |
|---|---|---|
| Rows | 200 | the count line says truncated |
| Result size | one answer of 80 KiB, JSON-encoded: the header, the skipped-vaults line and each row as a line of text | the count line says truncated |
| Time | 3 seconds, waiting for a slot included | `timeout` |
| Queries per user | one at a time, per instance | the next waits, within its own 3 seconds |
| Queries in all | two at a time, per instance, across all users | waits up to 1 second, then `unavailable` |
| Memory | a budget shared by all queries (`sqlite.query_memory_mib`) | `too_large` |
| Vaults | 10 | `invalid` |

The sandbox also stops reading at 1 MiB, which bounds memory. The two shared slots mean two slow queries hold
up everyone else's for a few seconds. That's acceptable at a team's size, and few slots keep few queries
sharing the memory budget.

## Errors

| Code | When |
|---|---|
| `invalid` | An empty query; a statement that isn't `SELECT`, `WITH` or `VALUES` ("only SELECT statements are allowed"); bad SQL (SQLite's message, with internal names replaced by vault names and file paths removed); more than one statement; something the sandbox doesn't allow ("not allowed in queries: ..."); more than 10 vaults |
| `timeout` | The query ran past 3 seconds, or the user's previous query was still running. Narrow it down, or use `search` |
| `too_large` | The query needed more memory than queries may use, or built a value over a size limit. Select fewer or shorter values |
| `unavailable` | Two queries were already running; speki is short of memory; a vault was removed or reopened; a named vault is building, failed or gone |
| `not_found` | A named vault the user can't read |
