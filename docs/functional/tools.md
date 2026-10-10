# Tools

Every tool returns structured content (JSON matching an output schema) and a short text rendering of the
same result, for clients that only show text.

Vaults are named `owner/repo`, matched without regard to letter case as on GitHub; results always use the name as GitHub spells it. Naming a vault twice is the same as naming it once. Paths are relative to the vault root. Wherever a tool takes `vaults`, leaving
it out means every vault the user can read.

## Versions

`read`, `search`, `list` and `query` return each document's `version`: a fingerprint of its content (the
git blob SHA, 40 hex characters). Tools that change a document use it to make sure nobody overwrites a
change they haven't seen:

- `write` replacing a document, `move` and `delete` require the version.
- `edit` takes it optionally. Without it, the rule that `old_text` must match exactly once is the guard.
- When the version doesn't match, the tool fails with `conflict` and gives the current version. Read the
  document again and redo the change.

Changes elsewhere in the vault don't cause conflicts. Only the document itself counts.

## Errors

A failed tool call returns an error text that starts with a code, then says what happened and what to do:

| Code | Meaning |
|---|---|
| `not_found` | No such vault or document, or the user can't see it. Use `search` or `list` to find it |
| `conflict` | The document changed since the given version. Includes the current version |
| `already_exists` | `write` without a version, or `move` onto a path that's taken. Includes the current version |
| `invalid` | Bad path, bad frontmatter, `old_text` not found or not unique, bad SQL. Says what and where |
| `no_write_access` | The user can read this vault but not push to it on GitHub |
| `read_only_session` | The user signed in read-only. Ask them to reconnect speki with write access |
| `vault_read_only` | GitHub refused the commit because the branch requires pull requests or checks |
| `secret_detected` | GitHub's push protection found a secret in the content. Remove it and tell the user |
| `too_large` | Over a limit. Says which one and how to stay under it |
| `timeout` | A query ran longer than 3 seconds |
| `rate_limited` | Too many requests. Says how long to wait |
| `unavailable` | Something went wrong on speki's side or GitHub's. Includes a request ID for the operator |

A user who isn't allowed in at all (not in a required organisation, or no vaults) doesn't get as far as a
tool. Their client gets an HTTP 403 with the reason.

## guide

What speki is and how to use it well. Agents call this once when they start working with speki.

Input: none.

Output: a longer explanation than the handshake instructions (what to search for, when to write, the
document format, link style, how versions work), then the user's vaults. Each vault comes with its title,
description, guidelines and whether the user can write to it.

## list

Without `vault`: the user's vaults.

```json
{ "vaults": [
  { "vault": "eve-online-tools/kb", "title": "Infrastructure knowledge",
    "description": "How our services are deployed and operated...",
    "can_write": true, "documents": 412 }
] }
```

With `vault`: the folders and documents under `path`.

| Input | Type | Default |
|---|---|---|
| `vault` | string | |
| `path` | folder | the root |
| `recursive` | bool | false |
| `cursor` | from a previous call | |

```json
{ "folders": ["runbooks/", "services/"],
  "documents": [
    { "path": "README.md", "title": "About this vault", "description": "...",
      "status": "active", "updated": "2026-10-09T14:02:11Z",
      "version": "3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1" }
  ],
  "next_cursor": null,
  "guidelines": "Put one document per service under services/ ..." }
```

At most 200 entries per call, sorted by path. The cursor stays valid when documents are added or removed
in between. The vault's guidelines come along when listing its root.

## search

Full-text search across the user's vaults, best match first.

| Input | Type | Default |
|---|---|---|
| `query` | words, `"exact phrases"`, `-excluded` words | required |
| `vaults` | list of vault names | all readable |
| `path` | only documents under this folder | |
| `tags` | documents must have all of them | |
| `exclude_deprecated` | bool | false. Deprecated documents are included but ranked lower |
| `limit` | 1 to 50 | 10 |

Search is forgiving. "configuring" finds "configured", "kubernets" finds "kubernetes", and "doctr" finds
"doctrine". Matches in titles, aliases and tags count for more than matches in the body. See
[search-and-query.md](search-and-query.md).

```json
{ "results": [
  { "vault": "eve-online-tools/kb", "path": "services/esi-proxy.md",
    "title": "ESI proxy", "description": "...", "tags": ["esi", "proxy"],
    "status": "active", "updated": "...", "version": "...",
    "snippet": "... the proxy **caches** ESI responses for ...", "score": 12.4 }
],
  "corrected": { "kubernets": "kubernetes" },
  "relaxed": false }
```

`corrected` lists words speki couldn't find and what it searched for instead. `relaxed` is true when no
document matched all words and the results match only some of them.

## schema

Describes the views `query` can use: columns with types and meaning, example queries, and the limits. No
input. The content is the "Query" section of [search-and-query.md](search-and-query.md).

## query

Runs one read-only SQL `SELECT` (SQLite dialect) over the user's vaults.

| Input | Type | Default |
|---|---|---|
| `sql` | string | required |
| `vaults` | list of vault names, at most 10 | all readable, if that's 10 or fewer |

A user who can read more than 10 vaults has to name the ones to query.

```json
{ "columns": ["vault", "path", "updated"],
  "rows": [["eve-online-tools/kb", "services/esi-proxy.md", "2026-10-09T14:02:11Z"]],
  "truncated": false }
```

Limits: 500 rows, 1 MiB of result, 3 seconds, one query per user at a time. Hitting the row or size limit
sets `truncated`. Hitting the time limit is a `timeout` error.

## read

| Input | Type | Default |
|---|---|---|
| `vault` | string | required |
| `path` | string | required |
| `from_line` | 1-based line number | 1 |
| `line_count` | number of lines | as many as fit in 1,000 lines and 64 KiB |

```json
{ "vault": "eve-online-tools/kb", "path": "services/esi-proxy.md",
  "version": "3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1",
  "metadata": { "title": "ESI proxy", "tags": ["esi", "proxy"], "status": "active" },
  "content": "---\ntitle: ESI proxy\n...",
  "from_line": 1, "line_count": 212, "total_lines": 212 }
```

`content` is the raw file, frontmatter included, so an agent can change it with `edit`. Line numbers count
from the top of the file.

When the document is longer than what was returned, `outline` lists its headings with line numbers (the
first 200), so the agent can read the part it needs. Otherwise the key is left out. A single line longer
than 64 KiB is cut, with a warning.

`warnings` lists frontmatter problems in documents changed outside speki, and is left out when there are
none.

## write

Creates a document, or replaces one entirely. To change part of a document, use `edit`: `write` replaces
the whole file, so writing back a partial read loses the rest.

| Input | Type | Default |
|---|---|---|
| `vault` | string | required |
| `path` | string | required |
| `content` | full markdown, with frontmatter | required |
| `version` | the version being replaced | omit when creating |
| `message` | one-line commit message | `Create <path>` or `Update <path>` |

Without `version`, `write` only creates, and fails with `already_exists` if the path is taken. With
`version`, it only replaces, and fails with `conflict` if the document changed.

Speki checks the frontmatter, sets `created` (new documents) and `updated`, and commits.

```json
{ "version": "...", "commit": "...", "index_pending": false }
```

`index_pending` is true in the rare case that the commit succeeded but speki's index hadn't caught up after
10 seconds. Searches may miss the change for a little while.

## edit

Replaces an exact piece of text in a document.

| Input | Type | Default |
|---|---|---|
| `vault`, `path` | string | required |
| `old_text` | exact text to replace, may include frontmatter, not empty | required |
| `new_text` | replacement | required |
| `replace_all` | bool | false |
| `version` | the version the agent read | optional |
| `message` | one-line commit message | `Update <path>` |

`old_text` has to appear exactly once, unless `replace_all` is set. Whitespace and line endings count. If it
appears zero or several times, the edit fails with `invalid` and says how many matches there were.

The result must still be a valid document. Speki updates `updated` and commits. The result has the same
shape as `write`'s.

## move

| Input | Type | Default |
|---|---|---|
| `vault` | string | required |
| `from`, `to` | paths | required |
| `version` | version of `from` | required |
| `message` | one-line commit message | `Move <from> to <to>` |

One commit removes `from` and adds `to`. Speki doesn't rewrite links in other documents. The result lists
the documents that linked to the old path, so the agent can fix them.

```json
{ "version": "...", "commit": "...", "index_pending": false,
  "linked_from": ["services/index.md", "runbooks/deploy.md"] }
```

## delete

| Input | Type | Default |
|---|---|---|
| `vault`, `path` | string | required |
| `version` | the version being deleted | required |
| `message` | one-line commit message | `Delete <path>` |

```json
{ "commit": "...", "index_pending": false, "linked_from": ["services/index.md"] }
```

The document stays in git history.

## history

| Input | Type | Default |
|---|---|---|
| `vault` | string | required |
| `path` | string | whole vault |
| `limit` | 1 to 50 | 10 |

```json
{ "commits": [
  { "sha": "9c1e...", "author": "noorbergen", "date": "2026-10-09T14:02:11Z",
    "message": "Update services/esi-proxy.md", "client": "claude.ai" }
] }
```

`client` names the MCP client that made the change through speki, from the `Speki-Client` line speki adds
to its commits. It's empty for commits made outside speki.

## Commits

Commits are authored by the user. Speki adds a trailer naming the client:

```text
Update services/esi-proxy.md

Speki-Client: claude.ai
```
