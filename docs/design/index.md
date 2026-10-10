# Search index

Package: `index`. Driver: `github.com/mattn/go-sqlite3`, built with `-tags sqlite_fts5,sqlite_omit_load_extension`.
[The search engine research](../research/search-engine.md) explains both choices.

`index` works on SQLite files. It knows nothing about vaults, users or git. `library` gives it changes and
says which files to search, and `knowledge` decides who may see what.

## Files

One index per vault, under `<data_dir>/<repo id>/`, in WAL mode. Each full build writes a new generation
(`index-1.db`, `index-2.db`, ...), and the newest complete one is the live one. Incremental changes are
applied to the live generation in place.

```go
type Index struct {
	current atomic.Pointer[generation]
}

type generation struct {
	path   string
	writer *sql.DB // MaxOpenConns(1), used by Apply
	reader *sql.DB // mode=ro, a pool of 4, used by Search, List, Document
	vocab  *vocabulary
	refs   sync.WaitGroup // requests and sandbox attachments using this generation
}
```

Switching generations swaps the pointer. The old generation is closed and its files (with their `-wal` and
`-shm`) deleted once the requests using it are done. A file is never renamed or replaced while open. That's
one of the ways SQLite databases get corrupted.

`Open` keeps the newest generation that passes `PRAGMA quick_check` and has the current schema version, and
deletes the rest, including leftovers of builds that were interrupted.

## Schema

```sql
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
-- 'schema' = schema version, 'commit' = commit the index reflects

CREATE TABLE documents (
	id          INTEGER PRIMARY KEY,
	path        TEXT NOT NULL UNIQUE,    -- exactly as in git
	name        TEXT NOT NULL,           -- lowercased file name without .md, for wikilinks
	title       TEXT NOT NULL,
	description TEXT,
	status      TEXT NOT NULL,           -- draft | active | deprecated
	tags        TEXT NOT NULL,           -- JSON array
	aliases     TEXT NOT NULL,           -- JSON array
	related     TEXT NOT NULL,           -- JSON array
	created     TEXT,
	updated     TEXT,
	extra       TEXT NOT NULL,           -- JSON object of unknown frontmatter keys
	headings    TEXT NOT NULL,           -- heading texts joined by newlines, for FTS
	body        TEXT NOT NULL,           -- markdown without frontmatter
	bytes       INTEGER NOT NULL,
	lines       INTEGER NOT NULL,
	version     TEXT NOT NULL,           -- git blob SHA
	warnings    TEXT NOT NULL            -- JSON array of frontmatter problems
);
CREATE INDEX documents_name ON documents (name);
CREATE INDEX documents_updated ON documents (updated);

CREATE TABLE tags (
	doc_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	tag    TEXT NOT NULL,
	PRIMARY KEY (doc_id, tag)
) WITHOUT ROWID;
CREATE INDEX tags_tag ON tags (tag);

CREATE TABLE links (
	doc_id    INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	target    TEXT NOT NULL,             -- as written
	candidate TEXT,                      -- path it would resolve to, or lowercased wikilink name
	kind      TEXT NOT NULL,             -- markdown | wikilink | related
	text      TEXT
);
CREATE INDEX links_candidate ON links (candidate);

CREATE TABLE headings (
	doc_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	level  INTEGER NOT NULL,
	text   TEXT NOT NULL,
	line   INTEGER NOT NULL
);

-- Ranked search, stemmed. Column order matches the bm25 weights.
CREATE VIRTUAL TABLE fts USING fts5 (
	title, aliases, tags, description, headings, body,
	content = 'documents', content_rowid = 'id',
	tokenize = 'porter unicode61 remove_diacritics 2',
	prefix = '2 3'
);

-- Substring matches on names.
CREATE VIRTUAL TABLE names USING fts5 (
	title, aliases, path,
	content = 'documents', content_rowid = 'id',
	tokenize = 'trigram'
);

-- Unstemmed vocabulary for typo correction. No positions, so it stays small.
CREATE VIRTUAL TABLE words USING fts5 (
	title, aliases, tags, description, headings, body,
	content = 'documents', content_rowid = 'id',
	tokenize = 'unicode61 remove_diacritics 2',
	detail = none
);
CREATE VIRTUAL TABLE words_vocab USING fts5vocab (words, 'row');
```

Links are resolved when read, through a view, so adding a document fixes the links pointing at it without
touching other rows:

```sql
CREATE VIEW resolved_links AS
SELECT l.doc_id, l.target, l.kind, l.text,
       CASE l.kind
         WHEN 'wikilink' THEN (SELECT path FROM documents d WHERE d.name = l.candidate
                               ORDER BY length(d.path), d.path LIMIT 1)
         ELSE (SELECT path FROM documents d WHERE d.path = l.candidate)
       END AS target_path
FROM links l;
```

The shortest path wins for ambiguous wikilinks, which is close to what Obsidian does.

The schema version is a constant in the code. An index with another version is rebuilt. There are no
migrations: rebuilding is always possible.

## Building and applying changes

```go
type Change struct {
	Path    string
	Version string // blob SHA; empty when the file was deleted
	Content []byte
}

// Open opens the newest usable generation in dir. Commit() is empty when there is none.
func Open(dir string) (*Index, error)
func (x *Index) Commit() string
func (x *Index) Apply(ctx context.Context, commit string, changes []Change) error
func (x *Index) Build(ctx context.Context, commit string, files iter.Seq2[Change, error]) error
```

`Apply` runs in one transaction. For each change it parses the content with `document.Parse`, deletes the
old row (cascading to tags, links and headings, and removing it from the FTS tables with the
external-content delete command) and inserts the new one. A change that fails (a path that isn't UTF-8,
a constraint error) is skipped and logged with its path. It never fails the whole transaction, so one bad
file can't stop a vault from updating. `meta.commit` is updated in the same transaction. Readers see the old
state or the new one, never a mix.

`Build` writes the next generation with `journal_mode=OFF` and `synchronous=OFF`: it inserts all rows,
runs the three FTS `rebuild` commands and `optimize`, switches to WAL, and then becomes the current
generation. Documents are parsed in parallel (one worker per CPU) and inserted by a single writer.

After either one, the in-memory vocabulary is reloaded from `words_vocab` in the background, one reload
at a time per vault.

A document whose frontmatter doesn't parse is still indexed: its body is searchable, its title comes from
the first heading or the file name, and the problem is stored in `warnings`. Only a file that isn't UTF-8
or whose frontmatter block is over 64 KiB is skipped.

## Search

```go
func (x *Index) Search(ctx context.Context, q Query) ([]Hit, error)
```

`index` searches one vault. `knowledge` calls it for each vault the user may read (through `library`,
8 at a time) and merges the results.

1. Parse the query string: words, quoted phrases, `-excluded` words. Lowercase and NFC-normalise.
2. Correct words. For each word, ask `words_vocab` whether it exists, in the same read transaction as the
   search, so a word added by the agent's own write a moment ago counts. A word that exists in none of the
   searched vaults is replaced by the best candidate from the in-memory vocabularies: edit distance 1 for
   words up to 5 letters, 2 for longer ones, highest document count wins. A trigram map over each vocabulary
   keeps candidate lookups well under a millisecond. The in-memory vocabulary may be a moment behind, which
   only means a brand-new word isn't suggested as a correction yet.
3. Build the FTS5 expression: each word quoted, the last one with `*`, phrases as phrases, joined with `AND`,
   exclusions with `NOT`. Tag, status and path-prefix filters go into the SQL, not the expression.
4. Run:

   ```sql
   SELECT d.path, d.title, d.description, d.tags, d.status, d.updated, d.version,
          snippet(fts, 5, '**', '**', '…', 32) AS snippet,
          bm25(fts, 10, 10, 6, 4, 3, 1) * CASE d.status WHEN 'deprecated' THEN 0.5 ELSE 1 END AS score
   FROM fts JOIN documents d ON d.id = fts.rowid
   WHERE fts MATCH ?
   ORDER BY score LIMIT ?;
   ```

   plus a query on `names` for substring hits on the raw words, scored below any full-text hit in a title.
5. `knowledge` merges per vault, then across vaults by score (bm25 is negative, lower is better; the result
   shows its absolute value), and keeps the top `limit`.
6. If there are no results and the query had more than one word, it runs again with `OR` and marks the
   result as relaxed.

## Listing and reading

`List(prefix, after, limit)` returns folders and documents under a folder in one sorted stream, starting
after a given path (keyset pagination), so a cursor stays valid while documents change. `Document(path)`
returns the metadata and version. `knowledge` reads the raw bytes from the mirror by version, so the index
doesn't store each file twice.

`LinkedFrom(path)` returns the documents with a link whose `candidate` is that path, or whose wikilink
name resolves to it. `knowledge` calls it before a `move` or `delete` commits, while the target still
exists.

## The query sandbox

```go
func NewSandbox() (*Sandbox, error)
func (s *Sandbox) Query(ctx context.Context, sources []Source, sql string) (Rows, error)
```

`Source` is the same `{Name, Index}` that search takes. `knowledge` checks every vault against the `Grant`
and passes at most 10 sources (more is `invalid`, with a message saying to pass `vaults`). The sandbox
holds a reference on each index's current generation until the query ends, and attaches that
generation's file.

1. Wait for a connection from the sandbox pool: a dedicated `*sql.DB` on `:memory:` with 4 open
   connections and `SetMaxIdleConns(0)`, so a connection is never reused. Each user has at most one query
   running. Waiting counts against the 3-second limit.
2. Through `sql.Conn.Raw`, on the `*sqlite3.SQLiteConn` (the setup runs before the authorizer exists):
   1. Check that `PRAGMA database_list` shows only `main` and `temp`. Anything else is a bug: drop the
      connection and fail.
   2. `ATTACH 'file:<path>?mode=ro' AS v0` ... `v9`, one per source, in a stable order.
   3. Create the `TEMP` views `documents`, `tags`, `links`, `headings`, `fts`. Each is a `UNION ALL` with a
      literal `vault` column per attached database. The views exist for convenience. They are not a
      security boundary: attached tables can be read directly, and that's fine, because only the user's own
      vaults are attached.
   4. `PRAGMA temp_store = MEMORY` and `cache_size = -8000` (8 MB) before the views, because changing
      `temp_store` throws the temp database away, views included. `PRAGMA query_only = 1` after them.
   5. Limits: `SQLITE_LIMIT_ATTACHED` to the number attached, `LENGTH` 100,000, `COLUMN` 50,
      `SQL_LENGTH` 100,000, `EXPR_DEPTH` 50, `COMPOUND_SELECT` 10, `LIKE_PATTERN_LENGTH` 100.
   6. Register the authorizer (below).
3. Before any of that, reject the SQL unless its first word is `SELECT`, `WITH` or `VALUES`, and if
   anything but whitespace and comments follows the first statement. The first-word rule also covers
   statements the authorizer never sees, such as `VACUUM` and `EXPLAIN`. This check is speki's own scanner (strings, quoted names, both comment styles), because
   go-sqlite3 runs every `;`-separated statement and returns the last one's rows.
4. Run it with a context that ends at the 3-second limit. The driver interrupts SQLite when the context ends.
   This is the only bound on running time: SQLite has no limit on instructions executed.
5. Read at most 500 rows and 1 MiB, then stop and mark `truncated`. The 1 MiB is counted as the values
   will be JSON-encoded in the result, where `<`, `>`, `&` and control characters take six bytes each.
6. Close the connection, marked bad (`driver.ErrBadConn` from `Raw`) so the pool discards it. The idle
   limit alone isn't enough: `database/sql` hands a released connection straight to a waiting caller.

The four connections are shared by all users. With one query per user and 3 seconds each, four people
running slow queries hold up everyone else's queries for those seconds, which is fine at this team size.

The attached index tables can be read directly, not only through the views: the authorizer can't tell a
query's own reads of `v0.documents` from FTS5's reads of the same table, since go-sqlite3 doesn't pass
the view a read comes from. That's no leak, since only the user's own vaults are attached. `schema.md`
tells agents the views are the interface and the tables behind them may change.

Memory: the same C file sets `sqlite3_soft_heap_limit64` (512 MiB), so SQLite releases cache memory under
pressure, and `sqlite3_hard_heap_limit64` (1 GiB), so an allocation past that fails with `SQLITE_NOMEM`.
The hard limit is what stops one query from growing the process: in-memory sorts and recursive queries
don't spill to disk, and a sort of many 90 KB rows otherwise reaches gigabytes within the time limit. A
query that hits it fails with `too_large`. Both limits are for the whole process. Index builds stay far
below them on their own (about 40 MB at 50,000 documents), but a build that allocates while a query has
just used up the shared budget fails with `SQLITE_NOMEM`, and the vault is synced again at the next poll. The 100 KB value and 50 column limits bound how much one row can hold before the 1 MiB
result check sees it.

Running a `MATCH` through the `fts` view makes FTS5 read, on the attached database, `fts_idx`,
`fts_config` and the content table `documents`, and run `PRAGMA data_version`. Those are on the lists
above. The match has to name a column (`body MATCH 'x'`): the view has no hidden column named after the
table, so `fts MATCH 'x'` is a "no such column" error.

`DEFENSIVE` mode and `trusted_schema` can't be set through mattn's Go API. A small C file in `index`
registers an `sqlite3_auto_extension` hook that, only for connections whose main database is `:memory:`,
calls `sqlite3_db_config` with `SQLITE_DBCONFIG_DEFENSIVE` on and `SQLITE_DBCONFIG_TRUSTED_SCHEMA` off.
Index writers aren't affected, because `DEFENSIVE` would block the `journal_mode=OFF` builds use. The file
declares the few SQLite prototypes it needs itself. The symbols come from the SQLite that mattn compiles
into the binary.

### The authorizer

go-sqlite3 passes the authorizer the action and its first three arguments: for a read, the table, the
column and the database name. The fourth (the view or trigger a read comes from) isn't available.

Allow:

| Action | Condition |
|---|---|
| `SQLITE_SELECT` | always |
| `SQLITE_READ` | `json_each` and `json_tree` in `main` (table-valued functions); a table of the index schema (`documents`, `tags`, `links`, `headings`, `resolved_links`) or one of the `fts` shadow tables (`fts_data`, `fts_idx`, `fts_config`, `fts_docsize`), in `v0`..`v9`; the five `temp` views by name; or an empty database name (CTE references) |
| `SQLITE_FUNCTION` | function on the allow-list |
| `SQLITE_RECURSIVE` | always. The time limit bounds it |
| `SQLITE_PRAGMA` | `data_version` with no argument. FTS5 runs it internally |

Everything else is denied, including `ATTACH`, `DETACH`, every `CREATE`, `DROP`, `INSERT`, `UPDATE`,
`DELETE`, `TRANSACTION`, `SAVEPOINT`, `ANALYZE`, `REINDEX` and any other pragma. `sqlite_master` and
`sqlite_temp_master` aren't on the read list.

The function allow-list: core scalar functions (`abs`, `coalesce`, `ifnull`, `iif`, `instr`, `length`,
`like`, `glob`, `lower`, `upper`, `ltrim`, `rtrim`, `trim`, `max`, `min`, `nullif`, `replace`, `round`,
`substr`, `substring`, `typeof`, `unicode`, `char`, `hex`, `quote`, `printf`, `format`), aggregates (`avg`,
`count`, `group_concat`, `string_agg`, `sum`, `total`), window functions, date and time functions, JSON
functions (`json`, `json_extract`, `json_each`, `json_tree`, `json_array_length`, `json_type`, `json_valid`,
`json_group_array`, `json_group_object`, `->`, `->>`), and `match` for FTS. Not allowed: `randomblob`,
`zeroblob`, `sqlite_*`, `load_extension`, `fts5_*` and anything else.

The setup in step 2 happens before the authorizer is registered, so speki's own `ATTACH` and `CREATE TEMP
VIEW` aren't affected by it. `load_extension` is compiled out by the build tag, so it couldn't run even if
the authorizer allowed it.

### Errors

| SQLite says | Tool sees |
|---|---|
| syntax error | `invalid`, with SQLite's message after replacing `v0`..`v9` with vault names and removing file paths |
| authorization denied | `invalid`: "not allowed in queries: <action>" |
| interrupted | `timeout` |
| string or blob too big, out of memory, other limits | `too_large` |
| anything else | `unavailable`, logged |
