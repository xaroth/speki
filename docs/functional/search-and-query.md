# Search and query

Speki offers two ways to find things. `search` takes plain words and returns the best matches, which is
what agents and people want most of the time. `query` takes SQL, for questions search can't answer:
"documents tagged `runbook` not updated this year", "what links to this page", "which documents have no
description".

Both only see the vaults the user can read.

## Search

### What the query string means

- Words are matched by their stem: "configuring", "configured" and "configuration" all match each other.
- The last word also matches as a prefix, so "doctr" finds "doctrine". This helps when only the start of a
  name is known.
- Words that appear nowhere in the user's vaults are replaced by the closest known words: "kubrenetes"
  becomes "kubernetes". The result says which words were corrected. A word that matches by its stem isn't
  corrected: "runs" finds "run".
- `"quoted phrases"` and `"quoted words"` must appear as written, after stemming: no prefix matching and no
  correction.
- `-word` excludes documents containing the word.
- All words have to match somewhere in the document (title, tags, aliases, description, headings or
  body). If that gives no results, speki tries again requiring any word, and sets `relaxed` in the result.

Titles, file names and aliases are also matched by substring, so "auth" finds a document titled
"OAuthProvider". Such matches rank below every full-text match.

### Ranking

Results are ranked with BM25, the usual relevance formula for text search: a word counts for more when it's
rare across the vault and when it makes up more of a short field. Matches are also weighted by where they
occur:

| Where | Weight |
|---|---|
| title, aliases | 10 |
| tags | 6 |
| description | 4 |
| headings | 3 |
| body | 1 |

A `deprecated` document's score is halved. A `draft` keeps its score.

Each vault has its own index and its own idea of which words are rare. So a hit in a small vault can rank
slightly higher than the same hit in a large one.

### Snippets

Each result has a snippet of up to 32 words from the body around the best match, with matched words in
`**bold**`. If the match was only in the title or tags, the snippet is the description.

## Query

`query` runs one SQLite `SELECT` against these views. Every view has a `vault` column, and `vault` plus
`path` identify a document.

### documents

| Column | Type | Meaning |
|---|---|---|
| `vault` | text | `owner/repo` |
| `path` | text | Path within the vault |
| `title` | text | Title, after the fallbacks in [documents.md](documents.md) |
| `description` | text | May be null |
| `status` | text | `draft`, `active` or `deprecated`, or `broken` for a file speki can't parse |
| `tags` | JSON array | Lowercased |
| `aliases` | JSON array | |
| `related` | JSON array | Paths as written |
| `created`, `updated` | text | ISO 8601 UTC, may be null for documents not written by speki |
| `extra` | JSON object | Frontmatter keys speki doesn't own |
| `body` | text | Markdown without the frontmatter |
| `bytes`, `lines` | integer | Size of the whole file |
| `version` | text | Git blob SHA |
| `broken` | text | Why the file can't be parsed. Null unless `status` is `broken`; a broken document has no body, tags or links |

### tags

One row per document and tag: `vault`, `path`, `tag`.

### links

One row per link found in a document, including `related` entries.

| Column | Meaning |
|---|---|
| `vault`, `path` | The document containing the link |
| `target` | The link as written |
| `target_path` | The document it points to in the same vault, or null when it points elsewhere or nowhere |
| `kind` | `markdown`, `wikilink` or `related` |
| `text` | Link text, if any |

### headings

`vault`, `path`, `level` (1 to 6), `text`, `line` (1-based, counted from the top of the file).

### fts

The full-text index, for combining text matching with other conditions. Columns: `vault`, `path`, `title`,
`aliases`, `tags`, `description`, `headings`, `body`. Match against a column, with
[FTS5 query syntax](https://sqlite.org/fts5.html#full_text_query_syntax): `WHERE fts.body MATCH 'cache'`.
Stemming applies, and a word ending in `*` matches as a prefix, but typo correction and ranking across
columns don't. Those are what `search` is for. Broken documents aren't in it.

### Examples

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

Documents that use a frontmatter key speki doesn't know:

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

### What's allowed

- One `SELECT` statement, `WITH` included. No `;`-separated statements.
- Core SQLite functions, JSON functions, date and time functions, and the usual aggregates and window
  functions.
- No `PRAGMA`, `ATTACH`, writes, or temporary tables.
- 500 rows, 1 MiB of results, 3 seconds, one query per user at a time. `truncated` tells you when rows
  were cut.
- A memory budget shared by all queries. A query that needs more fails with `too_large`; narrow it down.
- Two queries running at once across all users. Past that, a query waits up to a second, then fails with
  `unavailable`; try again in a few seconds.
- At most 10 vaults per query.

Bad SQL returns `invalid` with SQLite's message about the syntax. Speki strips names of internal tables
and files from those messages.
