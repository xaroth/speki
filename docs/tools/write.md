# write

Creates a document, or replaces one entirely, and commits it. What every change has in common is in
[changes.md](../changes.md).

`write` replaces the whole file. To change part of a document, use [`edit`](edit.md): writing back a partial
`read` loses the rest. The tool description says this first.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | required |
| `content` | the full markdown, with YAML frontmatter | required |
| `version` | the version being replaced | leave out when creating |
| `message` | one-line commit message. Only its first line is used, cut to 200 bytes | `Create <path>` or `Update <path>` |

## Create or replace

`version` decides which one the agent means, so it can't do the other by accident:

- Without `version`, `write` only creates. If the path holds a document, it fails with `already_exists` and
  that document's version. The agent then reads it and decides whether to replace it or edit it.
- With `version`, `write` only replaces. If the document is gone, it fails with `not_found`; if it changed,
  with `conflict`.

## What happens

When creating, the path must be one speki will write ([documents.md](../documents.md#paths)) and must not
collide with anything in the vault's git tree: a file speki doesn't index, a folder, the same name in other
letter case or another Unicode normalization. The check is against git, not the index, because the index
leaves out files it doesn't serve, and `write` must not overwrite those.

The content is parsed and checked: a new document needs a title (frontmatter or a `#` heading) and may have no
frontmatter problems at all; a replacement may not add problems the document didn't have. Frontmatter that
isn't valid YAML is always refused, even when the current document has it, because speki rewrites the block to
set `updated`. Tags or aliases already over their limit may not grow. Each `related`
entry the change adds must point to a document in the vault. Agents may only set `title`, `description`,
`tags`, `aliases`, `status`, `related` and the vault's `frontmatter.allow` keys; other keys must keep their
value ([documents.md](../documents.md#what-a-change-may-do)).

Speki then sets `created` (new documents) and `updated`, and commits.

## Output

```json
{ "version": "8d1f...", "commit": "4be0...", "index_pending": false }
```

The text says "Saved in commit ...", gives the new version, and adds "Search may not show the change for a
little while." when `index_pending` is true.

## Errors

| Code | When |
|---|---|
| `read_only_session`, `no_write_access` | The user can't write here |
| `already_exists` | Creating where something already is: a document (with its version), a file speki doesn't index, a folder, a file in the way of a folder in the path, or the same path in another Unicode normalization |
| `invalid` | A path speki won't write; a letter-case collision; bad frontmatter; a `related` entry that points nowhere; a locked key changed; no title |
| `not_found` | Replacing a document that doesn't exist. Leave out `version` to create it |
| `conflict` | The document changed since `version`, or kept changing while speki tried to commit |
| `broken` | A file that can't be parsed is at the path. Only `delete` works on it |
| `too_large` | Over 1 MiB, or frontmatter over 64 KiB once the dates are set |
| `vault_read_only`, `secret_detected`, `rate_limited` | GitHub refused the commit |
| `unavailable`, `timeout` | See [overview.md](overview.md#errors) |
