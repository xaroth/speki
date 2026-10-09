# Search engine

Researched 2026-10-09, with experiments and benchmarks run on Go 1.27.1.

What we need:

- Ranked full-text search (BM25) with English stemming, field weights, and snippets.
- Typo and partial-word tolerance.
- Agents running their own read-only SQL over documents, metadata, tags and links, without seeing vaults
  they can't read.
- Built by speki itself on disk it may lose, incrementally updated, one index per vault.
- Up to 20 vaults of up to 50,000 documents each.

## Engines

### SQLite with FTS5

FTS5 covers the search side: `porter` tokenizer for stemming, `bm25()` with a weight per column,
`snippet()` and `highlight()`, prefix indexes, a `trigram` tokenizer for substring matching, and
`fts5vocab` to read the vocabulary. External-content tables let one `documents` table feed several FTS
indexes. The rest of SQLite (JSON functions, window functions, views) covers the query side, and its
authorizer hook lets us restrict what agent SQL may do.

Measured on 50,000 synthetic documents of 5 KB (250 MB of text), single thread:

| Step | Time |
|---|---|
| Insert rows | 1.9 s |
| FTS5 porter index with `prefix='2 3'` | 32 s |
| Trigram index on title and path | 1.0 s |
| `optimize` | 4.9 s |
| Top-10 query with snippets | 7 to 200 ms, depending on how common the words are |

Those numbers are from the pure-Go driver. The cgo driver we picked is about 1.4 times faster at
building (below). The synthetic vocabulary is noisier than real English, so real indexes will be smaller.

### DuckDB

Ruled out. The Go driver needs cgo, and the FTS extension isn't bundled, so an offline container would
have to ship it separately. The index doesn't update when the table changes
([docs](https://duckdb.org/docs/current/core_extensions/full_text_search.html)), which means a full rebuild
on every write. It has no snippets, no prefix or fuzzy search, and no authorizer. DuckDB's security guide
itself says its settings are "not a substitute for proper sandboxing".

### Bleve

Pure Go, with BM25 since v2.5, fuzzy queries, boosts and highlighting. Its fuzzy matching is better than
anything SQLite has out of the box. But it can't answer SQL, so we'd run Bleve for search and SQLite for
`query`: two stores to build, keep in sync and rebuild on every start. It was also slower at indexing in
our test (5.3 MB/s against about 8 MB/s for FTS5). Not worth it for one feature we can build ourselves.

## SQLite drivers

The driver has to expose `sqlite3_set_authorizer` (see "Sandbox" below), and performance matters because
the index is speki's hot path.

| | mattn/go-sqlite3 | ncruces/go-sqlite3 | modernc.org/sqlite | zombiezen.com/go/sqlite |
|---|---|---|---|---|
| How | cgo | SQLite compiled to wasm, translated to Go (wasm2go), no runtime | C translated to Go | Wrapper over modernc |
| Authorizer | yes, sees function names | yes | no | yes, but can't see function names |
| Limits, interrupt on context | yes | yes | yes | yes |
| `DEFENSIVE` | through a small C hook | yes | connection option | yes |
| Maintenance | active, several contributors | active, one maintainer | very active | bug fixes only |

Benchmark: 10,000 documents of 5 KB, porter tokenizer, WAL, on a 6-core Ryzen 5600X:

| | mattn | ncruces | modernc | zombiezen |
|---|---|---|---|---|
| Bulk insert | 2.54 s | 3.32 s | 3.23 s | 3.24 s |
| FTS5 rebuild | 1.93 s | 2.80 s | 2.74 s | 2.70 s |
| Match + snippet, p50 / p95 | 6.8 / 8.4 ms | 15.1 / 19.2 ms | 14.6 / 18.1 ms | 14.3 / 17.8 ms |
| 8 connections, match + snippet | 930 q/s | 377 q/s | 282 q/s | 284 q/s |
| Sandbox setup per request (10 vaults) | 0.87 ms | 1.74 ms | 1.18 ms | 1.07 ms |
| Peak memory | 150 MB | 113 MB | 152 MB | 132 MB |

modernc and zombiezen scale poorly across connections because every allocation in `modernc.org/libc`
takes one process-wide lock, and FTS5 allocates a lot. Published benchmarks agree on the overall
picture: [cvilsmeier/go-sqlite-bench](https://github.com/cvilsmeier/go-sqlite-bench) (March 2026) and
[modernc's own scorecard](https://pkg.go.dev/modernc.org/sqlite-bench) (September 2026), which puts mattn
first and ncruces second. Results for ncruces from before March 2026 measure its old wazero runtime and
no longer apply.

Decision: mattn/go-sqlite3, built with `-tags sqlite_fts5`. It is two to seven times faster than the
alternatives and has everything the sandbox needs. The cost is cgo: a C compiler in CI and in the image
build, and slower builds. If that ever becomes a problem, ncruces is the pure-Go fallback with the same
capabilities.

## Typo tolerance

spellfix1 isn't bundled with mattn. We don't need it. The approach:

1. A second FTS5 table over the same text with the plain `unicode61` tokenizer and `detail=none`. It's
   small, because it stores no positions, and it gives `fts5vocab` the real, unstemmed words with
   document counts. FTS keeps it current, so we never maintain a word list by hand.
2. After each index update, speki loads the vocabulary into memory per vault (about 100,000 words).
3. A query word that isn't in any of the user's vaults is corrected to the most common known word within
   edit distance 1 (words up to 5 letters) or 2 (longer), using a trigram pre-filter so we don't compare
   against every word.

Feeding stemmed words into the corrector doesn't work. In the experiment, "replcation" became "rvlation".
The corrector has to work on real words.

Partial words: the last query word also matches as a prefix (`doctr*`), helped by `prefix='2 3'`.
Substrings inside names (`proxy` in `esi-proxy`) come from a small trigram index over title, aliases and
path.

## Sandbox

Agents' SQL has to stay inside the user's vaults. We tested two approaches.

An authorizer on one shared database, filtering on the view a read comes through, can be bypassed.
SQLite reports CTE names in the same place as view names, so
`WITH docs AS (SELECT * FROM main.documents) SELECT ...` passed for a CTE named after the permitted view,
and returned another vault's rows.

Physical isolation held up against everything we tried: a new `:memory:` connection per request that
attaches only the user's vault files read-only and builds `TEMP` views with `UNION ALL` over them. Then lock
it down:

- `PRAGMA query_only=1`, `DEFENSIVE`, attach limit set to the number attached
- limits on string length (1 MB), SQL length (100 KB), expression depth, compound selects. SQLite has no
  limit on instructions executed (`SQLITE_LIMIT_VDBE_OP` limits program size), so the timeout below is
  what bounds running time
- an allow-list authorizer: `SELECT`, reads of the views and their underlying tables, recursive CTEs, an
  allow-list of functions, the FTS shadow tables FTS5 reads while matching, and `PRAGMA data_version`
  with no argument, because FTS5 runs it internally during every match
- a context timeout that interrupts the query, a row cap enforced while reading rows

Blocked in testing: `ATTACH`, `DETACH`, `PRAGMA query_only=0`, reading `sqlite_master`, `INSERT`, `CREATE TEMP
TABLE`, `load_extension`, unlisted functions. A runaway recursive CTE and a three-way cross join were
interrupted at the timeout. `randomblob(1e8)` failed on the length limit.

FTS through the views works when matching on a column (`fts.body MATCH 'x'`), because SQLite pushes the
condition down into each vault. Matching on the table name and calling `bm25()` only work on the real
tables, so ranked search stays in the `search` tool, which runs per vault.

SQLite allows at most 10 attached databases by default. `query` therefore takes at most 10 vaults per call.
`search` fans out per vault in Go and has no such limit.

## Scores across vaults

Each vault has its own BM25 statistics, so the same match scores differently in a small vault than in a
large one. At our sizes we merge raw scores and accept the imprecision.
