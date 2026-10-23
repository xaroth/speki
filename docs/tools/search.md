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
| `repositories` | only vaults covering any of these, up to 10: git remote URLs, `host/owner/repo` or `owner/repo` | |
| `path` | only documents under this folder | |
| `tags` | documents must have all of them. Lowercased, leading `#` dropped | |
| `exclude_deprecated` | bool | false: deprecated documents are included, ranked lower |
| `limit` | 1 to 50 | 10 |

## Output

```text
No document has "kubrenetes"; searched for "kubernetes" instead.
<results boundary="9f3c1a7e5b2d8c40">
acme/kb services/edge-proxy.md  Edge proxy  updated 2026-10-09
How the edge proxy routes and caches requests to upstream services.
... the proxy **caches** upstream responses for ...

acme/kb runbooks/proxy-restart.md  Restarting the proxy [deprecated]  updated 2025-03-14
... drain the **proxy** before ...
</results boundary="9f3c1a7e5b2d8c40">
```

Notes come first, each on its own line, when they apply:

| Note | When |
|---|---|
| "Not searched, so this result says nothing about them: ..." | Readable vaults speki can't serve weren't searched. Only when `vaults` was left out ([overview.md](overview.md#vaults-speki-cant-serve)) |
| "Searched the vaults covering <repo> or <repo>: ..." | With `repositories`: the vaults that were searched |
| "No document has "x"; searched for "y" instead." | One per word no searched vault contains |
| "No document matched every word; these match some of them." | The search was relaxed ([search.md](../search.md#what-the-query-means)) |
| "No documents found. Try other words, or list a vault to browse it. If you work it out, write it back." | No hits |

Then all hits go in one `results` fence, best match first, ties sorted by vault, then path. Hits are
separated by a blank line, and each has:

1. The vault, the path, the title, the status in brackets when it isn't `active`, and the day it was last
   updated. The title is cut to 200 bytes.
2. The description, when there is one and it isn't the snippet, cut to 500 bytes.
3. The snippet: up to 32 words of the body around the best match, matches in `**bold**`. The description
   when the body didn't match, and for a substring hit. At most 500 bytes.

The hit carries no version: an agent reads a document before changing it, and `read` gives the version.
Order follows a score from 0 to 1, relative to the best match in the same vault, which isn't shown
([search.md](../search.md#ranking)).

## With `repositories`

Only the vaults whose `repositories` cover any of the given repositories are searched, narrowed further by
`vaults` if given, and the skipped-vaults note names only covering vaults. When no vault covers them,
nothing is searched and the text says "None of the vaults you can read covers <repo>, so nothing was
searched. Leave out repositories to search them anyway." ("you named" when `vaults` was given). Speki
doesn't fall back to every vault: those results would look as if they were about the repository.

This is how an agent working in a code checkout finds the knowledge about that code: pass every remote
URL from `git remote -v` as is. In a fork, `origin` is the fork and the vault names the upstream
repository; passing both matches either way. Normalizing is as for
[`list`](list.md#with-repositories-the-vaults-that-cover-them).

## Limits

`tags`, `path` and `exclude_deprecated` are applied inside the ranking, so a filtered search still returns up
to `limit` matching documents and stays fast. `path` matches the folder in Unicode NFC however it's typed.

A search has 5 seconds, well inside the request's 50, so a slow one can't hold a vault's few reader
connections for long. At most 8 vaults are searched at once, and a query may have at most 32 different
words or phrases.

## Errors

| Code | When |
|---|---|
| `invalid` | `limit` above 50 or negative (0 means 10); an empty query; only excluded words; more than 32 words; a malformed repository or more than 10 |
| `not_found` | A named vault the user can't read |
| `unavailable` | A named vault is building, failed or gone; a vault was removed or reopened during the search |
| `timeout` | The search ran past 5 seconds. Name fewer vaults, or narrow it with `path` or `tags` |
