# edit

Replaces an exact piece of text in a document and commits the change. It's the tool for changing part of
a document. What every change has in common (the write lock, commits, conflicts, retries, waiting for the
index) is in [changes.md](../changes.md).

The agent names the text it expects to find, not a line number. If the document changed so that the text
no longer appears, the edit fails instead of landing in the wrong place.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | required |
| `old_text` | the exact text to replace, as read. May include frontmatter. Not empty | required |
| `new_text` | the replacement | required |
| `replace_all` | replace every occurrence instead of requiring exactly one | false |
| `version` | the version the agent read | optional, recommended |
| `message` | one-line commit message. Only its first line is used, cut to 200 bytes | `Update <path>` |

## What happens

1. The user must be able to write to the vault in this session.
2. The current document is loaded. When `version` is given and doesn't match, the answer is `conflict` with
   the current version.
3. `old_text` is counted in the raw file, overlapping occurrences included, so `ab ab` in `ab ab ab` counts
   twice and is ambiguous. Zero matches, or more than one without `replace_all`, is `invalid`, with the
   count when there are several. With `replace_all`, every non-overlapping occurrence is replaced.
4. The result must still be a valid document, and must not add problems the document didn't have
   ([documents.md](../documents.md#what-a-change-may-do)). Frontmatter that isn't valid YAML is always
   refused, because speki rewrites the block to set `updated`. Speki sets `updated` and commits.

Because `edit` works on raw bytes, an agent can change frontmatter with it too, and speki still checks the
outcome.

## Why pass the version

Without `version`, the rule that `old_text` matches exactly once is the only guard. That doesn't stop a
retried edit from applying twice when its `new_text` still contains `old_text` and the first attempt's
answer was lost. Speki looks for a commit whose answer was lost before reporting a failure
([changes.md](../changes.md#lost-answers)), but an agent that gave up before that can still retry. With
`version`, the retry fails with `conflict`, because the first attempt already changed the document.

## Output

```text
Saved in commit 4be0c7a1d4f7b0e3a6c9d2f5b8e1a4c7d0f3b6e9.
New version: 8d1f0b3e6a9c2d5f8b1e4a7c0d3f6b9e2a5c8d1f
```

When the index hadn't caught up in time ([changes.md](../changes.md#after-the-commit)), a third line says
"Search may not show the change for a little while."

## Errors

| Code | When |
|---|---|
| `read_only_session`, `no_write_access` | The user can't write here ([changes.md](../changes.md#who-may-change-what)) |
| `invalid` | Empty `old_text`; `old_text` not found or not unique; the result isn't a valid document or adds a problem |
| `not_found` | No such vault or document |
| `conflict` | The document changed since `version`, or kept changing while speki tried to commit |
| `broken` | The file can't be parsed. Only `delete` works on it |
| `too_large` | The result is over 1 MiB, or its frontmatter over 64 KiB once the dates are set |
| `vault_read_only`, `secret_detected`, `rate_limited` | GitHub refused the commit |
| `unavailable`, `timeout` | See [overview.md](overview.md#errors). After `timeout`, read the document before retrying |
