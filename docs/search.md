# Search

`search` takes plain words and returns the best matching documents across the user's vaults. It's
full-text search with stemming, prefix matching and typo correction, built on SQLite's FTS5. There is no
semantic or embedding search. For questions about metadata and links rather than words, there's
[query](query.md). The tool's inputs and outputs are in [tools/search.md](tools/search.md).

## What the query means

- Words match by their stem: "configuring", "configured" and "configuration" all match each other. Stemming
  is English (Porter), and accents are ignored.
- The last word also matches as a prefix, so "doctr" finds "doctrine". This helps when only the start of a
  name is known.
- A word that no searched vault contains is replaced by the closest word that one does: "kubrenetes" becomes
  "kubernetes". The result says which words were corrected.
- `"quoted phrases"` and `"quoted words"` must appear as written, after stemming. They're never corrected or
  matched as a prefix. When the last term is quoted, nothing is matched as a prefix.
- `-word` excludes documents containing the word.
- Every word has to match somewhere in the document: title, aliases, tags, description, headings or body.
  When nothing matches every word and there was more than one, speki searches again requiring any word,
  and says so in the result.

Titles, aliases and file names are also matched by substring, so "auth" finds a document titled
"OAuthProvider" and "proxy" finds `edge-proxy.md`. Only file names count, not folders, or "ops" would match
everything under `ops/`. Substring matches score half the lowest full-text match in the same vault, and at
most 0.01, so they rank below every full-text match there. They need words of at least 3 letters.

## Typo correction

A word is only corrected when it's unknown: no searched vault has it as a word, and it doesn't match anything
by its stem either. So "runs" isn't corrected when documents say "run": correcting it to a nearby word would
override stemming. The check runs against the live index, so a word an agent wrote a moment ago counts.

Some words are never corrected: quoted and excluded words, words shorter than 3 letters, and words with
digits, because numbers aren't typos ("2026" shouldn't become "2025").

An unknown word is replaced by a known word within edit distance 1 for words up to 5 letters, or 2 for longer
ones, where swapping two neighboring letters counts as one edit. The closest word wins, then the one in the
most documents.

Correction uses the real words in the vaults, not their stems. Correcting against stems gives nonsense:
"replcation" became "rvlation" when tried. Each vault keeps a second, small full-text table with unstemmed
words, and speki holds its vocabulary in memory with a trigram index for fast lookups. The in-memory copy is
reloaded after each update and may be a moment behind, which only means a brand-new word isn't suggested as
a correction yet.

## Ranking

Results are ranked with BM25, the usual relevance formula for text search: a word counts for more when it's
rare across the vault and when it makes up more of a short field. Matches are weighted by where they occur:

| Where | Weight |
|---|---|
| title, aliases | 10 |
| tags | 6 |
| description | 4 |
| headings | 3 |
| body | 1 |

A `deprecated` document's score is halved, inside the ranking, so deprecated documents can't push active
ones out of the top results. A `draft` keeps its score. Broken documents aren't searchable.

Each vault has its own index and its own idea of which words are rare, so raw scores from different vaults
don't compare. Scores are therefore relative to the best match in the same vault: it scores 1 (0.5 if
deprecated), and the others score their share of it. Results from all vaults are merged on that score, then by
vault and path, so the best match of a small vault isn't buried under a large vault's weaker ones. A score says
how good a match is within its vault, not overall. Dividing by the best score keeps how much better one hit is
than the next within a vault, at the cost of treating every vault's best match as equally good.

## Snippets

Each result has a snippet of up to 32 words from the body around the best match, with matched words in
`**bold**`. When the body didn't match (only the title or tags did), the snippet is the description.

## Filters

`path` keeps documents under a folder, `tags` keeps documents that have all the given tags, and
`exclude_deprecated` drops deprecated ones. `repositories` searches only the vaults that cover any of the
given code repositories ([vaults.md](vaults.md#repositories)). Filters apply inside the ranking, each
answered by an index, so a filtered search ranks only up to the limit and never reads the bodies of
documents it filters out.

## Limits

A search has 5 seconds. Past that it fails with `timeout`; narrow it with fewer vaults, `path` or `tags`. A
query may hold at most 32 different words or phrases. Up to 8 vaults are searched in parallel, and the results
merged.

## Why SQLite FTS5

Speki needs ranked search with stemming, field weights and snippets; typo and partial-word tolerance; and
read-only SQL for agents, restricted to the vaults they can read. All of it built by speki itself, on disk it
may lose, updated incrementally, one index per vault.

FTS5 covers the search side: the Porter stemmer, BM25 with a weight per column, snippets, prefix indexes, a
trigram tokenizer for substrings, and access to the vocabulary for typo correction. The rest of SQLite
covers `query`: JSON functions, window functions, views, and an authorizer hook that restricts what agent SQL
may do. One file per vault holds all of it.

The alternatives considered:

- DuckDB's full-text extension isn't bundled, so an offline image would have to ship it. Its index doesn't
  update when the table changes, so every write would mean a rebuild. It has no snippets, prefix or fuzzy
  search, and no authorizer.
- Bleve has better fuzzy matching, but can't answer SQL. Speki would run Bleve for search and SQLite for
  `query`: two stores to build, keep in step and rebuild. It also indexed slower in testing.

The driver is mattn/go-sqlite3, which uses cgo. It was two to seven times faster than the pure-Go drivers
in testing, and exposes the authorizer the query sandbox needs. The cost is a C compiler in the build.
Extension loading is compiled out.

## Measured

Measured 2026-10-10 on generated vaults of documents of about 5 KB, limited to 4 cores.
`TestMeasure` in `library/index/build_internal_test.go` repeats these with `SPEKI_MEASURE=1`.

| What | Result |
|---|---|
| Build 50,000 documents | 43 to 54 s |
| Index size for 50,000 documents | 809 MB |
| Peak memory while building them | 40 MB |
| Apply one changed document | 19 ms |
| Search one vault of 10,000, p50 / p95 | 5 / 12 ms |
| Search one vault of 50,000, p50 / p95 | 26 / 56 ms |
| Search 10 vaults of 10,000, p50 / p95 | 16 / 27 ms |

Two choices drive these numbers. Ranking inside the full-text table and reading only the best candidates'
rows is about five times faster than joining every match, which reads each body from disk. Building the
full-text tables with one rebuild at the end, instead of row by row, nearly halves build time.

The generated text is harsher than real text: common words show up in most documents, so most queries match
thousands of them.
