# history

Lists the latest commits of a vault, or of one document: who changed it, when, why, and through which
client. Because every change through speki is a commit authored by the user, this is the audit log.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | the whole vault |
| `limit` | 1 to 50 | 10 |
| `cursor` | the cursor the previous call with the same `vault` and `path` ended with | |

## Output

```text
9c1e4b7a2d5f8e0c3b6a9d2e5f8c1b4a7d0e3f6c 2026-10-09T14:02:11Z octocat via claude.ai: Add cache TTLs
4b17e2c9a0d3f6b8e1c4a7d0f3b6e9c2a5d8f1b4 2026-10-08T09:00:00Z hubot: Create services/edge-proxy.md
More commits follow: call history again with cursor "9c1e4b7a2d5f8e0c3b6a9d2e5f8c1b4a7d0e3f6c:2".
```

One line per commit, newest first: the full SHA, the date in UTC, the author, "via" the client when there
is one, and the message. The author is the GitHub login, or the git author name for a commit not linked
to an account, cut to 100 bytes. The message is the commit's first line, cut to 300 bytes. The client
names the MCP client that made the change through speki, read from the last `Speki-Client` trailer of the
message ([changes.md](../changes.md#commits)), cut to 64 bytes. Commits made outside speki have none.

When older commits follow, the text ends with "More commits follow: call history again with cursor
"..."." Passing that as `cursor`, with the same `vault` and `path`, gets the next page. Pages continue
from the branch head the first page was read at, so commits made in between don't shift them. With no
commits it says "No commits found."

## How it works

Speki's git copies are shallow and have no history, so `history` asks GitHub for the branch's commits that
touched the path, with the user's own token. GitHub checks the user's access for this call too. The answer
doesn't depend on speki's index.

A cursor is `<head>:<skip>`: the full SHA of the branch head when the first page was read, and how many
commits came before the next page. Later pages list from that head, which is what keeps them stable. Listing
from the last commit shown instead would skip commits from merged branches that are older than it but not
its ancestors. For the whole vault, the head is the first commit listed; for one document, speki looks up the
branch head first.

## Errors

| Code | When |
|---|---|
| `invalid` | `limit` above 50 or negative (0 means 10); a cursor `history` didn't return, or whose commit GitHub doesn't know |
| `not_found` | No such vault the user can read |
| `unavailable` | The vault is building, failed or gone; GitHub no longer accepts the user's sign-in; GitHub failed |
