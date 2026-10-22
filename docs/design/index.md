# Search index

Package: `index`. Driver: `github.com/mattn/go-sqlite3`, built with `-tags sqlite_fts5,sqlite_omit_load_extension`.
[The search engine research](../research/search-engine.md) explains both choices.

`index` works on SQLite files. It knows nothing about vaults, users or git. `library` gives it changes,
and `knowledge` decides who may see what and which indexes to search.

## Files

One index per vault, under `<data_dir>/<repo id>/index/`, in WAL mode. Each full build writes a new
generation (`index-1.db`, `index-2.db`, ...), and the newest complete one is the live one. Incremental
changes are applied to the live generation in place.

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
	key         TEXT NOT NULL,           -- path in NFC, which links are matched against (indexed)
	name        TEXT NOT NULL,           -- file name without .md, folded (document.LinkName), for wikilinks
	title       TEXT NOT NULL,
	description TEXT,
	status      TEXT NOT NULL,           -- draft | active | deprecated | broken
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
	warnings    TEXT NOT NULL,           -- JSON array of frontmatter problems
	broken      TEXT                     -- why the file can't be parsed, when status is broken
);
CREATE INDEX documents_name ON documents (name);
CREATE INDEX documents_updated ON documents (updated);
CREATE INDEX documents_deprecated ON documents (id) WHERE status = 'deprecated'; -- for ranking
CREATE INDEX documents_broken ON documents (id) WHERE status = 'broken';         -- left out of search

CREATE TABLE tags (
	doc_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	tag    TEXT NOT NULL,
	PRIMARY KEY (doc_id, tag)
) WITHOUT ROWID;
CREATE INDEX tags_tag ON tags (tag);

CREATE TABLE links (
	doc_id    INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
	target    TEXT NOT NULL,             -- as written
	candidate TEXT,                      -- path it would resolve to, or lowercased wikilink name, in NFC
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
                               ORDER BY d.status = 'broken', length(d.key), d.path LIMIT 1)
         ELSE (SELECT path FROM documents d WHERE d.key = l.candidate
               ORDER BY d.status = 'broken', d.path LIMIT 1)
       END AS target_path
FROM links l;
```

Paths and names compare in NFC, so a link typed in NFC finds a file macOS named in NFD, and the other way
round. Names fold case after normalising, so letters that lowercase differently composed and decomposed
(İ) still match. The `path` filter of `search` compares in NFC too. `target_path` is the path as stored
in git.

The shortest path wins for ambiguous wikilinks, which is close to what Obsidian does. A document that
isn't broken wins over a broken one first, so one unparseable file doesn't capture links meant for a good
one. A link whose only match is broken still resolves to it, and `LinkedFrom` matches the same way.

The schema version is a constant in the code. An index with another version is rebuilt. There are no
migrations: rebuilding is always possible.

## Building and applying changes

```go
type Change struct {
	Path    string
	Version string // blob SHA; empty when the file was deleted
	Content []byte
}

// Open opens the newest usable generation in dir, creating dir if needed. Commit() is empty when there
// is none.
func Open(ctx context.Context, dir string, logger *slog.Logger) (*Index, error)
func (x *Index) Commit() string
func (x *Index) Apply(ctx context.Context, commit string, changes []Change) error
func (x *Index) Build(ctx context.Context, commit string, files iter.Seq2[Change, error]) error
```

`Apply` runs in one transaction. For each change it parses the content with `document.Parse`, deletes the
old row (cascading to tags, links and headings, and removing it from the FTS tables with the
external-content delete command) and inserts the new one. A file that's unusable in itself (not UTF-8,
frontmatter over 64 KiB) is logged with its path and indexed as broken, so one bad file can't stop a vault
from updating. Any other failure aborts the whole transaction and the next sync tries again.
`meta.commit` is updated in the same transaction. Readers see the old state or the new one, never a mix.

`Build` indexes unusable files the same way. A broken row has the file's new version, size and line
count, its path as title, `status = 'broken'`, the parse error in `broken`, and no other content: no
body, headings, tags or links. Broken rows stay out of `fts`, `names` and `words`: the triggers skip
them, and `Build` deletes them from the three tables after its rebuilds, which read every row. So their
file-name titles don't match in search or the sandbox, and don't feed typo correction. The sandbox `fts`
view also filters them, because a scan of an external-content table without `MATCH` reads `documents`
directly. The index never keeps an older version of a file that changed, because `knowledge` checks an
agent's version against the index, and a stale row would let a write overwrite a change nobody could
read. Agents see the parse error verbatim, so its text is stable; the reasons are listed in
[tools.md](../functional/tools.md#errors).

`Build` writes the next generation with `journal_mode=OFF` and `synchronous=OFF`: it inserts all rows,
runs the three FTS `rebuild` commands and `optimize`, switches to WAL, and then becomes the current
generation. Documents are parsed in parallel (one worker per CPU) and inserted by a single writer.

After either one, the in-memory vocabulary is reloaded from `words_vocab` in the background, one reload
at a time per vault.

A document whose frontmatter doesn't parse is still indexed: its body is searchable, its title comes from
the first heading or its path, and the problem is stored in `warnings`. Only a file that isn't UTF-8
or whose frontmatter block is over 64 KiB is broken.

## Search

```go
type Source struct {
	Name  string // the vault's name, which hits carry
	Index *Index
}

func Search(ctx context.Context, sources []Source, text string, opt Options) (Results, error)
```

`Search` is a package function over several indexes. `knowledge` passes one `Source` per vault to search,
each checked against the `Grant`, and `index` searches them in parallel (8 at a time) and merges the
results itself. It needs all of them at once anyway, because typo correction looks at every searched
vault's vocabulary.

1. Parse the query string: words, quoted phrases, `-excluded` words. Lowercase and NFC-normalise.
2. Correct words. For each word, ask `words_vocab` whether it exists, in the same read transaction as the
   search, so a word added by the agent's own write a moment ago counts. A word that exists in none of the
   searched vaults is replaced by the best candidate from the in-memory vocabularies: edit distance 1 for
   words up to 5 letters, 2 for longer ones, highest document count wins. A word that matches anything by
   its stem (`runs` where documents say `run`) isn't corrected, and quoted words never are. A trigram map
   over each vocabulary keeps candidate lookups well under a millisecond. The in-memory vocabulary may be
   a moment behind, which only means a brand-new word isn't suggested as a correction yet.
3. Build the FTS5 expression: each word quoted, the last unquoted one with `*`, phrases as phrases, joined
   with `AND`, and the exclusions as one group: `(...) NOT ("a" OR "b")`. Repeated words count once; more
   than 32 distinct words or phrases is `invalid`. Tag, status and path-prefix filters go into the SQL, not
   the expression.
4. Rank inside the full-text table, deprecated documents halved there through a partial index on them,
   so they can't push an active document out of the top, and join only the best. Broken documents are
   left out through a partial index on them too, in the ranking and in the `names` query. Filters apply
   inside the ranking as well, each a list of IDs from an index (`tags_tag`, `documents_key`, the
   deprecated partial index), so a filtered search also ranks only up to the limit and never reads the
   bodies of documents it filters out. Each filter is written on `+rowid`: given a plain `rowid IN (...)`,
   SQLite runs the full-text match once per listed ID instead.

   ```sql
   WITH ranked AS (
     SELECT rowid AS id, bm25(fts, 10, 10, 6, 4, 3, 1) *
            CASE WHEN rowid IN (SELECT id FROM documents WHERE status = 'deprecated') THEN 0.5 ELSE 1 END AS rank
     FROM fts WHERE fts MATCH ? AND rowid NOT IN (SELECT id FROM documents WHERE status = 'broken')
       AND +rowid IN (SELECT doc_id FROM tags WHERE tag = ?) -- one line per filter
     ORDER BY rank LIMIT ?)
   SELECT d.path, d.title, ..., ranked.rank AS score FROM ranked JOIN documents d ON d.id = ranked.id
   ORDER BY score;
   ```

   plus a query on `names` (titles, aliases and file names, not folders) for substring hits on the raw
   words. Those rank below every full-text hit in their vault.
5. Merge the hits of every vault by score (bm25 is negative, lower is better; the result shows its
   absolute value), and keep the top `limit`.
6. If there are no results and the query had more than one word, it runs again with `OR` and marks the
   result as relaxed.

A search has 5 seconds (`SearchTimeout`), well inside the request's 50, so a slow one can't hold a vault's
4 reader connections for long. `knowledge` turns running out of it into a `timeout` error.

## Listing and reading

`List(prefix, after, limit)` returns folders and documents under a folder in one sorted stream, starting
after a given path (keyset pagination), so a cursor stays valid while documents change. `Document(path)`
returns the metadata and version. `knowledge` reads the raw bytes from the mirror by version, so the index
doesn't store each file twice. Broken documents are listed, with their status, and `Document` returns them
with `Broken` set; `knowledge` answers `read` on one with `broken` instead of content.

`LinkedFrom(path)` returns the documents with a link that resolves to that path, by the same rules as
`resolved_links`. `knowledge` calls it before a `move` or `delete` commits, while the target still
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

1. Take one of 2 query slots, shared by all users, waiting at most 1 second; after that the query fails
   as busy. Then refuse to start, also as busy, if SQLite's memory is already past the query budget
   (below). Then take a connection from the sandbox pool: a dedicated `*sql.DB` on `:memory:` with 2 open
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
   statements the authorizer never sees, such as `VACUUM` and `EXPLAIN`. This check is speki's own
   scanner (strings, quoted names, both comment styles), because go-sqlite3 runs every `;`-separated
   statement and returns the last one's rows.
4. Run it with a context that ends at the 3-second limit. The driver interrupts SQLite when the context ends.
   This is the only bound on running time: SQLite has no limit on instructions executed.
5. Read at most 500 rows and 1 MiB, then stop and mark `truncated`. The 1 MiB is the rows as they'll go
   out, measured with `json.Marshal` on each value: escapes for `<`, `>` and `&`, for invalid UTF-8 and
   line separators, numbers in full, separators and column names included. The text rendering beside
   them has its own 1 MiB cap and says when it shows fewer rows than the structured part.
6. Close the connection, marked bad (`driver.ErrBadConn` from `Raw`) so the pool discards it. The idle
   limit alone isn't enough: `database/sql` hands a released connection straight to a waiting caller.

The two slots are shared by all users. Two people running slow queries hold up everyone else's queries
for those seconds, which is fine at this team size. Few slots keep few queries sharing the memory budget.

The attached index tables can be read directly, not only through the views: the authorizer can't tell a
query's own reads of `v0.documents` from FTS5's reads of the same table, since go-sqlite3 doesn't pass
the view a read comes from. That's no leak, since only the user's own vaults are attached. `schema.md`
tells agents the views are the interface and the tables behind them may change.

Memory: `index.SetMemoryLimits`, called by `service.New` from `sqlite.memory_limit_mib` (default 1 GiB)
and `sqlite.query_memory_mib` (default 384 MiB), sets `sqlite3_hard_heap_limit64`, so an allocation past
the limit fails with `SQLITE_NOMEM`, and `sqlite3_soft_heap_limit64` at half of it, so SQLite releases
cache memory under pressure. The hard limit is what stops one query from growing the process: in-memory
sorts and recursive queries don't spill to disk, and a sort of many 90 KB rows otherwise reaches gigabytes
within the time limit. Both limits are for the whole process, so queries are stopped well before the hard
limit: the hardening hook below also sets a progress handler on sandbox connections that, every 1,000
instructions, interrupts the query once SQLite's memory passes the query budget. That leaves the rest for
searches and index builds. A query interrupted while its context is still live was stopped by the budget
and fails with `too_large`. The
check counts all of SQLite's memory, so a query can be stopped for another's usage; the 2 slots keep that
rare.

Index builds stay far below the limits on their own (about 40 MB at 50,000 documents), but a build that
allocates while something else holds the memory can still fail with `SQLITE_NOMEM`. The build or apply
then fails as a whole and the index stays at its commit. The error shows on the status page, but the
vault doesn't back off more with each failure: it's synced again after a fixed minute. Only a file that's unusable in itself is indexed as
broken instead. The 100 KB value and 50 column limits bound how much one row can hold before the 1 MiB
result check sees it.

Running a `MATCH` through the `fts` view makes FTS5 read, on the attached database, `fts_idx`,
`fts_config` and the content table `documents`, and run `PRAGMA data_version`. Those are on the lists
above. The match has to name a column (`body MATCH 'x'`): the view has no hidden column named after the
table, so `fts MATCH 'x'` is a "no such column" error.

`DEFENSIVE` mode and `trusted_schema` can't be set through mattn's Go API. The cgo preamble of
`library/index/defensive.go` registers an `sqlite3_auto_extension` hook that, only for connections whose main
database is `:memory:`, calls `sqlite3_db_config` with `SQLITE_DBCONFIG_DEFENSIVE` on and
`SQLITE_DBCONFIG_TRUSTED_SCHEMA` off, and sets the memory budget's progress handler. Index writers aren't affected, because `DEFENSIVE` would block the
`journal_mode=OFF` builds use. The preamble declares the few SQLite prototypes it needs itself. The symbols
come from the SQLite that mattn compiles into the binary.

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
| interrupted at the time limit | `timeout` |
| interrupted at the memory budget, string or blob too big, out of memory, other limits | `too_large` |
| no free query slot, or SQLite's memory past the budget before the query starts (setup included) | `unavailable`: try again in a few seconds |
| anything else | `unavailable`, logged |
