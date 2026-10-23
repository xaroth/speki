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

```json
{ "columns": ["vault", "path", "updated"],
  "rows": [["acme/kb", "services/esi-proxy.md", "2026-10-09T14:02:11Z"]],
  "truncated": false,
  "skipped_vaults": [{ "vault": "acme/new", "state": "building" }] }
```

`columns` and `rows` are never null. Binary values come back as text, and NaN and infinity as strings.
`truncated` is true when rows were cut at 200 rows or at the size of one answer. `skipped_vaults` works as in `search`: readable vaults
that weren't covered because speki can't serve them, when `vaults` was left out.

The text rendering is tab-separated, header first, with `NULL` for nulls, then a line with the row count.
When rows were cut it adds ", truncated: add a LIMIT or narrow the query to see the rest". Skipped vaults
follow on their own line ("Not queried, ..."). It isn't fenced; each cell is put on one line and escaped
instead ([overview.md](overview.md#results)). The text and `rows` always hold the same rows.

## Limits

| Limit | Value | When reached |
|---|---|---|
| Rows | 200 | `truncated` |
| Result size | one answer of 80 KiB, JSON-encoded, each row counted twice: in `rows` and as a line of text | `truncated` |
| Time | 3 seconds, waiting for a slot included | `timeout` |
| Queries per user | one at a time, per instance | the next waits, within its own 3 seconds |
| Queries in all | two at a time, per instance, across all users | waits up to 1 second, then `unavailable` |
| Memory | a budget shared by all queries (`sqlite.query_memory_mib`) | `too_large` |
| Vaults | 10 | `invalid` |

The sandbox also stops reading at 1 MiB, which bounds memory. The two shared slots mean two slow queries hold up everyone else's for a few seconds. That's acceptable at a
team's size, and few slots keep few queries sharing the memory budget.

## Errors

| Code | When |
|---|---|
| `invalid` | An empty query; a statement that isn't `SELECT`, `WITH` or `VALUES` ("only SELECT statements are allowed"); bad SQL (SQLite's message, with internal names replaced by vault names and file paths removed); more than one statement; something the sandbox doesn't allow ("not allowed in queries: ..."); more than 10 vaults |
| `timeout` | The query ran past 3 seconds, or the user's previous query was still running. Narrow it down, or use `search` |
| `too_large` | The query needed more memory than queries may use, or built a value over a size limit. Select fewer or shorter values |
| `unavailable` | Two queries were already running; speki is short of memory; a vault was removed or reopened; a named vault is building, failed or gone |
| `not_found` | A named vault the user can't read |
