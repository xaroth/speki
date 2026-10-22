# search

Full-text search across the user's vaults, best match first. It's the tool agents should reach for first
whenever a question may already be answered by the team's knowledge, which is why it's always loaded.
How words are matched and ranked is described in [search.md](../search.md); this page is the tool's
contract.

## Input

| Input | Type | Default |
|---|---|---|
| `query` | words, `"quoted phrases"`, `-excluded` words | required |
| `vaults` | list of vault names | every readable vault |
| `repository` | only vaults covering this repository: a git remote URL, `host/owner/repo` or `owner/repo` | |
| `path` | only documents under this folder | |
| `tags` | documents must have all of them. Lowercased, leading `#` dropped | |
| `exclude_deprecated` | bool | false: deprecated documents are included, ranked lower |
| `limit` | 1 to 50 | 10 |

## Output

```json
{ "results": [
  { "vault": "acme/kb", "path": "services/esi-proxy.md",
    "title": "ESI proxy", "description": "...", "tags": ["esi", "proxy"],
    "status": "active", "updated": "2026-10-09T14:02:11Z",
    "version": "3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1",
    "snippet": "... the proxy **caches** ESI responses for ...", "score": 12.4 }
],
  "corrected": { "kubrenetes": "kubernetes" },
  "relaxed": false,
  "skipped_vaults": [{ "vault": "acme/new", "state": "building" }] }
```

| Field | Meaning |
|---|---|
| `results` | Hits, highest `score` first. Ties sort by vault, then path |
| `snippet` | Up to 32 words of the body around the best match, matches in `**bold**`. The description when the body didn't match. At most 500 bytes |
| `corrected` | Words no searched vault contains, and what speki searched for instead |
| `relaxed` | True when no document matched every word, and these results match only some |
| `skipped_vaults` | Readable vaults that weren't searched because speki can't serve them. Only when `vaults` was left out |
| `repository_vaults` | With `repository`: the vaults that were searched |

The text rendering puts notes first (skipped vaults, the repository's vaults, corrections, relaxed), then
all hits in one `results` fence: per hit a line with vault, path, title, its status when not `active`, and
version, then the snippet. With no hits it suggests other words, or browsing with `list`.

## With `repository`

Only the vaults whose `repositories` cover the repository are searched, narrowed further by `vaults` if
given, and `skipped_vaults` lists only covering vaults. When no vault covers it, nothing is searched and the
text says so. Speki doesn't fall back to every vault: those results would look as if they were about the
repository.

This is how an agent working in a code checkout finds the knowledge about that code: pass `git remote
get-url origin` as is.

## Limits

A search has 5 seconds, well inside the request's 50, so a slow one can't hold a vault's few reader
connections for long. At most 8 vaults are searched at once, and a query may have at most 32 different
words or phrases.

## Errors

| Code | When |
|---|---|
| `invalid` | `limit` outside 1 to 50; an empty query; only excluded words; more than 32 words; a malformed `repository` |
| `not_found` | A named vault the user can't read |
| `unavailable` | A named vault is building, failed or gone; a vault was removed or reopened during the search |
| `timeout` | The search ran past 5 seconds. Name fewer vaults, or narrow it with `path` or `tags` |
