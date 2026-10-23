# delete

Deletes a document. It stays in git history. What every change has in common is in
[changes.md](../changes.md).

The tool description tells agents to prefer `status: deprecated` for knowledge that is outdated but still
worth finding. A deprecated document still turns up in search, ranked lower. A deleted one is gone from
search, and only `history` and git have it. `delete` is marked destructive, so clients ask the user first.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | required |
| `version` | the version being deleted, from `read` | required |
| `message` | one-line commit message. Only its first line is used, cut to 200 bytes | `Delete <path>` |

## What happens

1. The user must be able to write to the vault in this session, and `version` must be given.
2. The document must exist at `version`.
3. Speki collects the documents that link to it, then commits the deletion.

`delete` is the one change allowed on a broken document, a file speki can't parse. No agent can have read
such a file through speki, so it can't be edited, but a file that was never meant to be a document can be
cleared out with the version the `broken` error gives.

## Output

```json
{ "commit": "4be0...", "index_pending": false, "linked_from": ["services/index.md"] }
```

The text says "Deleted in commit ..." and lists the linking documents under "These documents link to the old
path:". There's no `version`: the document is gone. `linked_from` lists the documents whose links now point
nowhere, so the agent can fix them with `edit`.

## Errors

| Code | When |
|---|---|
| `read_only_session`, `no_write_access` | The user can't write here |
| `invalid` | No `version` |
| `not_found` | No such vault or document |
| `conflict` | The document changed since `version`, or the vault kept changing while speki tried to commit |
| `vault_read_only`, `rate_limited` | GitHub refused the commit |
| `unavailable`, `timeout` | See [overview.md](overview.md#errors) |
