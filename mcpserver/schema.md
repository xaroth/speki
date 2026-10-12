# Querying speki

`query` runs one read-only SQLite `SELECT` (a `WITH` is fine) over your vaults. Every view has a `vault`
column (`owner/repo`), and `vault` plus `path` identify a document. Use it for questions `search` can't
answer: documents by metadata, links between documents, gaps in the knowledge.

## Views

documents: one row per document.

- `vault`, `path`
- `title`; `description` (may be null); `status`: `draft`, `active` or `deprecated`, or `broken` for a
  file speki can't parse
- `tags`, `aliases`, `related`: JSON arrays. Tags are lowercased; related holds paths as written
- `created`, `updated`: ISO 8601 UTC text, may be null for documents not written by speki
- `extra`: JSON object of frontmatter keys speki doesn't own
- `body`: markdown without the frontmatter
- `bytes`, `lines`: size of the whole file
- `version`: git blob SHA
- `broken`: why the file can't be parsed, null otherwise. A broken document has no body, tags or links

tags: one row per document and tag: `vault`, `path`, `tag`.

links: one row per link in a document, including `related` entries.

- `vault`, `path`: the document containing the link
- `target`: the link as written
- `target_path`: the document it points to in the same vault, or null when it points elsewhere or nowhere
- `kind`: `markdown`, `wikilink` or `related`
- `text`: link text, if any

headings: `vault`, `path`, `level` (1 to 6), `text`, `line` (counted from 1 at the top of the file).

fts: the full-text index: `vault`, `path`, `title`, `aliases`, `tags`, `description`, `headings`, `body`.
Match against a column with FTS5 syntax: `WHERE body MATCH 'cache AND invalidation'`. Stemming applies,
and `cach*` matches as a prefix; typo correction and ranking don't. Those are what `search` is for.

`MATCH` can't be pushed through the view when the query aggregates or groups the matches, which fails with
"unable to use function MATCH in the requested context". Match in a subquery or a materialized CTE first:

    SELECT count(*) FROM (SELECT path FROM fts WHERE body MATCH 'cache' LIMIT 500);

    WITH m AS MATERIALIZED (SELECT vault, path FROM fts WHERE body MATCH 'cache')
    SELECT vault, count(*) FROM m GROUP BY vault;

Query these views. The tables behind them can be read too, but they're speki's internals and change
without notice.

## Examples

Runbooks nobody touched this year:

    SELECT d.vault, d.path, d.updated
    FROM documents d JOIN tags t USING (vault, path)
    WHERE t.tag = 'runbook' AND (d.updated IS NULL OR d.updated < '2026-01-01')
    ORDER BY d.updated;

What links to a document:

    SELECT vault, path, text FROM links
    WHERE vault = 'owner/repo' AND target_path = 'services/esi-proxy.md';

Links that point at nothing:

    SELECT vault, path, target FROM links
    WHERE kind != 'related' AND target_path IS NULL AND target NOT LIKE 'http%';

A frontmatter key speki doesn't know:

    SELECT vault, path, json_extract(extra, '$.owner') AS owner
    FROM documents WHERE json_extract(extra, '$.owner') IS NOT NULL;

Text search combined with metadata:

    SELECT f.vault, f.path, d.updated
    FROM fts f JOIN documents d USING (vault, path)
    WHERE f.body MATCH 'cache AND invalidation' AND d.status = 'active'
    ORDER BY d.updated DESC LIMIT 20;

## Limits

- One statement. Core, JSON, date and time functions, aggregates and window functions. No `PRAGMA`,
  `ATTACH`, writes or temporary tables.
- 500 rows and 1 MiB of results (and 1 MiB more for the same rows as text); `truncated` says when rows
  were cut. 3 seconds, one query at a time.
- At most 10 vaults per query. If you can read more, name them in `vaults`.
