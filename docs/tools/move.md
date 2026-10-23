# move

Moves or renames a document in one commit. What every change has in common is in
[changes.md](../changes.md).

`move` is marked destructive, so clients ask the user first: the document disappears from its old path, and
links to it break.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `from` | the document's current path | required |
| `to` | the new path, ending in `.md` | required |
| `version` | the version of `from`, from `read` | required |
| `message` | one-line commit message. Only its first line is used, cut to 200 bytes | `Move <from> to <to>` |

## What happens

1. The user must be able to write to the vault in this session, and `version` must be given.
2. `from` must exist at `version`. `to` must be a path speki will write and must be free, checked against the
   vault's git tree as for a new document ([write.md](write.md#what-happens)).
3. Speki collects the documents that link to `from`, while it still exists.
4. One commit deletes `from` and adds `to` with the same bytes.

The content is copied unchanged: `updated` isn't touched and the frontmatter isn't checked, so the version
stays the same. Moving a document doesn't change what it says.

## Links

Speki doesn't rewrite links in other documents: the commit changes only `from` and `to`. The result lists
the documents that linked to the old path, so the agent can fix them with `edit`, each as its own change.
Links are matched the way the `links` view resolves them ([query.md](../query.md#links)), so relative
markdown links, wikilinks and `related` entries all count.

## Output

```json
{ "version": "3f2a...", "commit": "4be0...", "index_pending": false,
  "linked_from": ["services/index.md", "runbooks/deploy.md"] }
```

The text says "Moved in commit ...", gives the version, and lists the linking documents under "These
documents link to the old path:".

## Errors

| Code | When |
|---|---|
| `read_only_session`, `no_write_access` | The user can't write here |
| `invalid` | No `version`; `from` and `to` are the same path, also when they differ only in Unicode normalization; `to` isn't a path speki will write; `to` differs from an existing path only in letter case |
| `not_found` | No such vault or document at `from` |
| `already_exists` | `to` is taken by a document (with its version), a file speki doesn't index, a folder, or the same path in another Unicode normalization |
| `conflict` | `from` changed since `version`, or the vault kept changing while speki tried to commit |
| `broken` | `from` can't be parsed. Only `delete` works on it |
| `vault_read_only`, `secret_detected`, `rate_limited` | GitHub refused the commit |
| `unavailable`, `timeout` | See [overview.md](overview.md#errors) |
