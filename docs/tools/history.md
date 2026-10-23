# history

Lists the latest commits of a vault, or of one document: who changed it, when, why, and through which
client. Because every change through speki is a commit authored by the user, this is the audit log.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | the whole vault |
| `limit` | 1 to 50 | 10 |
| `cursor` | `next_cursor` from the previous call with the same `vault` and `path` | |

## Output

```json
{ "commits": [
  { "sha": "9c1e4b7a...", "author": "noorbergen", "date": "2026-10-09T14:02:11Z",
    "message": "Update services/esi-proxy.md", "client": "claude.ai" }
],
  "next_cursor": "4b17...:10" }
```

Newest first. `author` is the GitHub login, or the git author name for a commit not linked to an account,
cut to 100 bytes. `message` is the commit's first line, cut to 300 bytes. `client` names the MCP client that
made the change through speki, read from the last `Speki-Client` trailer of the message
([changes.md](../changes.md#commits)), cut to 64 bytes. It's left out for commits made outside speki.

When older commits follow, `next_cursor` is set: pass it as `cursor`, with the same `vault` and `path`, to get
the next page. It's left out on the last page. Pages continue from the branch head the first page was read
at, so commits made in between don't shift them.

The text rendering is one line per commit: SHA, date, author, "via" the client when there is one, and the
message. It says "No commits found." when there are none, and ends with "More commits follow: call history
again with cursor "..."." when more follow.

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
| `invalid` | `limit` outside 1 to 50; a cursor `history` didn't return, or whose commit GitHub doesn't know |
| `not_found` | No such vault the user can read |
| `unavailable` | The vault is building, failed or gone; GitHub no longer accepts the user's sign-in; GitHub failed |
