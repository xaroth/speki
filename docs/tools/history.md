# history

Lists the latest commits of a vault, or of one document: who changed it, when, why, and through which
client. Because every change through speki is a commit authored by the user, this is the audit log.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | the whole vault |
| `limit` | 1 to 50 | 10 |

## Output

```json
{ "commits": [
  { "sha": "9c1e4b7a...", "author": "noorbergen", "date": "2026-10-09T14:02:11Z",
    "message": "Update services/esi-proxy.md", "client": "claude.ai" }
] }
```

Newest first. `author` is the GitHub login, or the git author name for a commit not linked to an account,
cut to 100 bytes. `message` is the commit's first line, cut to 300 bytes. `client` names the MCP client that
made the change through speki, read from the last `Speki-Client` trailer of the message
([changes.md](../changes.md#commits)). It's left out for commits made outside speki.

The text rendering is one line per commit: SHA, date, author, "via" the client when there is one, and the
message.

## How it works

Speki's git copies are shallow and have no history, so `history` asks GitHub for the branch's commits that
touched the path, with the user's own token. GitHub checks the user's access for this call too. The answer
doesn't depend on speki's index.

## Errors

| Code | When |
|---|---|
| `invalid` | `limit` outside 1 to 50 |
| `not_found` | No such vault the user can read |
| `unavailable` | The vault is building, failed or gone; GitHub no longer accepts the user's sign-in; GitHub failed |
