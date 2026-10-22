# Query

`query` runs one read-only SQL `SELECT` over the user's vaults. It's for questions search can't answer:
"documents tagged `runbook` not updated this year", "what links to this page", "which documents have no
description". Agents learn the views from the [`schema`](tools/schema.md) tool; the tool's inputs and
limits are in [tools/query.md](tools/query.md).

These questions are about metadata and links, not words. The same SQLite file per vault that serves search
holds everything a query needs.

## Views

Every view has a `vault` column, and `vault` plus `path` identify a document. The views are the interface.
The tables behind them can be read too, but they're speki's internals and change without notice.

### documents

| Column | Type | Meaning |
|---|---|---|
| `vault` | text | `owner/repo` |
| `path` | text | Path in the vault |
| `title` | text | Title, after the fallbacks in [documents.md](documents.md#frontmatter) |
| `description` | text | May be null |
| `status` | text | `draft`, `active` or `deprecated`, or `broken` for a file speki can't parse |
| `tags` | JSON array | Lowercased |
| `aliases` | JSON array | |
| `related` | JSON array | Paths as written |
| `created`, `updated` | text | ISO 8601 UTC. May be null for documents not written by speki |
| `extra` | JSON object | Frontmatter keys speki doesn't own |
| `body` | text | Markdown without the frontmatter |
| `bytes`, `lines` | integer | Size of the whole file |
| `version` | text | Git blob SHA |
| `broken` | text | Why the file can't be parsed. Null unless `status` is `broken`; a broken document has no body, tags or links |

### tags

One row per document and tag: `vault`, `path`, `tag`.

### links

One row per link in a document, `related` entries included.

| Column | Meaning |
|---|---|
| `vault`, `path` | The document containing the link |
| `target` | The link as written |
| `target_path` | The document it points to in the same vault, or null when it points elsewhere or nowhere |
| `kind` | `markdown`, `wikilink` or `related` |
| `text` | Link text, if any |

`target_path` is worked out when the view is read, so a link to a document that didn't exist yet resolves
as soon as the document does. Markdown links and `related` paths match by path, in Unicode NFC, so a link
typed on one system finds a file named on another. Wikilinks match by file name, ignoring case: a document
that parses wins over a broken one, so one bad file can't capture links meant for a good one, and among
those the shortest path wins.

### headings

`vault`, `path`, `level` (1 to 6), `text`, `line` (counted from 1 at the top of the file).

### fts

The full-text index, for combining text matching with other conditions: `vault`, `path`, `title`,
`aliases`, `tags`, `description`, `headings`, `body`. Match against a column, with
[FTS5 query syntax](https://sqlite.org/fts5.html#full_text_query_syntax): `WHERE fts.body MATCH 'cache'`.
Stemming applies, and a word ending in `*` matches as a prefix. Typo correction and ranking across columns
don't: those are what `search` is for. Broken documents aren't in it.

`MATCH` can't be used when the query aggregates or groups the matches. Match in a subquery or a
materialized CTE first:

```sql
WITH m AS MATERIALIZED (SELECT vault, path FROM fts WHERE body MATCH 'cache')
SELECT vault, count(*) FROM m GROUP BY vault;
```

## Examples

Runbooks nobody touched this year:

```sql
SELECT d.vault, d.path, d.updated
FROM documents d JOIN tags t USING (vault, path)
WHERE t.tag = 'runbook' AND (d.updated IS NULL OR d.updated < '2026-01-01')
ORDER BY d.updated;
```

What links to a document:

```sql
SELECT vault, path, text FROM links
WHERE vault = 'acme/kb' AND target_path = 'services/esi-proxy.md';
```

Links that point at nothing:

```sql
SELECT vault, path, target FROM links
WHERE kind != 'related' AND target_path IS NULL AND target NOT LIKE 'http%';
```

A frontmatter key speki doesn't know:

```sql
SELECT vault, path, json_extract(extra, '$.owner') AS owner
FROM documents WHERE json_extract(extra, '$.owner') IS NOT NULL;
```

Text search combined with metadata:

```sql
SELECT f.vault, f.path, d.updated
FROM fts f JOIN documents d USING (vault, path)
WHERE f.body MATCH 'cache AND invalidation' AND d.status = 'active'
ORDER BY d.updated DESC LIMIT 20;
```

## What's allowed

- One statement starting with `SELECT`, `WITH` or `VALUES`. Nothing but whitespace and comments after it.
- Core SQLite functions, JSON functions, date and time functions, aggregates and window functions, and
  recursive CTEs.
- No `PRAGMA`, `ATTACH`, writes or temporary tables, and no `randomblob`, `zeroblob`, `load_extension` or
  `sqlite_*` functions.
- 500 rows and 1 MiB of results, 3 seconds, one query per user at a time, at most 10 vaults.

## The sandbox

Agent SQL must never see a vault the user can't read. Speki's indexes are separate files, and that's what the
sandbox relies on.

An authorizer filter on one shared database, allowing reads only through permitted views, was tested and
bypassed: SQLite reports a CTE's name where it reports a view's, so a CTE named after a permitted view read
another vault's rows. Physical isolation held up against everything tried. So every query gets:

1. A fresh in-memory SQLite connection, checked to have nothing attached, used for this one query and then
   thrown away.
2. Only the user's own vault index files attached, read-only, at most 10 (SQLite's default attach limit).
   Speki's own views are created over them before any of the agent's SQL runs.
3. `query_only`, SQLite's defensive mode, and untrusted schemas.
4. Limits on value size (100 KB), columns (50), SQL length (100 KB), expression depth (50), compound
   selects (10) and `LIKE` patterns (100 bytes).
5. Its own statement scanner, which rejects more than one statement. The driver otherwise runs every
   `;`-separated statement and returns the last one's rows, and some statements never reach the authorizer.
6. An allow-list authorizer: `SELECT`, reads of the index tables and of the full-text tables' internals in
   the attached files, an allow-list of functions, and the one pragma FTS5 runs internally. Everything else is
   denied, including `sqlite_master`.
7. A 3-second limit that interrupts the query. SQLite has no limit on instructions executed, so time is the
   only bound on a runaway recursive CTE.
8. Row and size caps enforced while reading results.
9. Errors with file paths removed and internal database names replaced by vault names.

Extension loading isn't compiled into the binary, so `load_extension` couldn't run even if every layer above
failed.

The attached tables can be read directly, not only through the views. The authorizer can't tell a query's
own reads from FTS5's reads of the same tables. That's no leak, since only the user's own vaults are attached.

## Memory

In-memory sorts and recursive queries don't spill to disk, and without a limit a sort of large rows reaches
gigabytes within 3 seconds. So SQLite has a hard memory limit for the whole process (`sqlite.memory_limit_mib`,
1 GiB by default), past which allocations fail, and releases cache memory past half of it.

Queries are stopped well before that. Every 1,000 steps, a running query checks SQLite's total memory, and
once it passes the query budget (`sqlite.query_memory_mib`, 384 MiB by default) the query fails with
`too_large`. That leaves the rest for searches and index updates. Since the check counts all of SQLite's
memory, a query can be stopped for another's usage; allowing only two queries at a time keeps that rare.
