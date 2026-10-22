# Changes

Every change an agent makes through `write`, `edit`, `move` or `delete` becomes one git commit on the
vault's default branch, made through GitHub's API with the user's own token. This page describes that
path: who may change what, how conflicts are caught, what a commit looks like, and how speki's copy catches
up.

## Who may change what

Before anything else, every change checks, in order:

1. The vault exists and the user can read it (otherwise `not_found`, or `unavailable` when it isn't served).
2. The user signed in with read and write access (otherwise `read_only_session`).
3. The user can push to the repository on GitHub and is in the vault's `writers` (otherwise
   `no_write_access`).

These come first so the answer doesn't depend on whether the rest of the request is well formed. Speki's
answer is a cached view of GitHub's, up to 5 minutes old. GitHub checks push access again when the commit
arrives, along with branch rules and push protection, whatever speki thought.

## Versions and conflicts

Speki uses optimistic concurrency: an agent passes the version it read, and the change only goes through if
the document still has that version. Nobody overwrites a change they haven't seen, and nothing is locked
while an agent thinks.

- `write` replacing a document, `move` and `delete` require the version. `write` without one only creates.
- `edit` takes it optionally. Its exact-text match already guards against most surprises, and the version
  closes the rest ([tools/edit.md](tools/edit.md#why-pass-the-version)).
- A version that doesn't match fails with `conflict` and the current version. The agent reads again and redoes
  its change.

Only the document itself counts. A change elsewhere in the vault doesn't cause a conflict, even though it
moves the branch head.

When the agent's version doesn't match speki's index, speki first syncs the vault once: the agent may have
read a version GitHub already has and the index doesn't yet. Only if it still doesn't match is the answer
`conflict`.

Before committing a change to an existing document, speki also compares the version in its index with the
blob in its git copy at the same commit. The two should never disagree. The check keeps a bug in the index
from turning into an overwritten change: a mismatch syncs once, and if it survives, the change fails with
`conflict`, the mismatch is logged with both SHAs, and `speki_write_guard_mismatches_total` counts it.

## One change at a time

Each instance runs one change per vault at a time. A change waits at most 15 seconds for the one before it,
then fails with `unavailable` ("another change is still being committed").

Between instances there's no lock. Each commit names the head commit it expects the branch to be at, and
GitHub refuses it if the branch moved. Speki then syncs and tries again, up to three times in all, as long as
the document itself didn't change. After three, the answer is `conflict`.

## Commits

A commit is made with GitHub's GraphQL `createCommitOnBranch` mutation, with the user's token:

- It takes several file changes at once, so `move` is one commit: a deletion and an addition.
- It's checked against the expected head commit, which turns a race into a clean error.
- GitHub signs it, so it shows as verified. The user is the author; GitHub is the committer.
- Push protection and branch rulesets apply to it.

GitHub's file contents API can't make a commit with more than one file in it, which rules it out for `move`.

The message is the first line of the agent's `message`, or a default (`Create <path>`, `Update <path>`,
`Move <from> to <to>`, `Delete <path>`), cut to 200 bytes. Speki then adds two trailers:

```text
Update services/esi-proxy.md

Speki-Client: claude.ai
Speki-Request: 5f0c2a9e41b7d3c8e6a1f092
```

`Speki-Client` names the MCP client the user signed in with, or `unknown`. A message line that looks like
this trailer is replaced by the default, so an agent can't claim to be another client. `history` reads the
last one. `Speki-Request` is random per attempt and is how speki finds a commit whose answer was lost
(below).

A file speki writes gets mode `100644`. An executable `.md` file loses its executable bit when speki changes
or moves it, because the mutation has no file modes.

## When GitHub refuses

| GitHub says | Agent sees |
|---|---|
| The branch moved | retried, then `conflict` |
| Push protection found a secret | `secret_detected`, with GitHub's description |
| A ruleset or branch protection forbids direct commits | `vault_read_only` |
| The user can't push | `no_write_access` |
| Slow down (secondary rate limit) | `rate_limited`, with the wait |
| The user's sign-in is no longer valid | `unavailable`: ask the user to reconnect |

Push protection, rulesets and missing permissions all come back from GitHub as the same error type, so speki
matches GitHub's messages first and only then falls back to the type. A refused commit is logged with
GitHub's full message, since the agent only gets the code. Speki never calls push protection's bypass.

## Lost answers

A commit whose answer was lost (a dropped connection, a timeout, a server error) may have landed anyway.
Reporting that as a failure invites a retry, and a retried `edit` can apply twice. So before reporting a
failure, speki lists the latest commits that touched the path, as the user, and looks for the commit's
`Speki-Request` trailer. If it's there, the change is reported as a success with that commit. GitHub
sometimes answers "this may be the result of a timeout" while still finishing the commit, so the lookup is
tried a second time after two seconds.

Any other GraphQL error counts as a definite refusal and isn't looked for. An agent that hung up before speki
could check can still retry; passing `version` to `edit` turns such a retry into a `conflict`.

When the call runs out of its 50 seconds, the agent gets `timeout`, which tells it to read the document
before retrying, because the change may have landed.

## After the commit

After a successful commit, speki:

1. computes the new version, the git blob SHA of the content it sent
2. syncs its copy of the vault and waits, at most 10 seconds, until the index has the change
3. tells other instances about the new head through Redis, if configured
4. answers with the version, the commit SHA and `index_pending`

So an agent that writes and then searches normally finds its own change. `index_pending: true` means the
commit landed but the index hadn't caught up in time, or the vault needs a full rebuild, which the poller
does in the background. Search and `read` may show the old state for a little while.

Steps 2 and 3 run as background work that outlives the request. When the request runs out of time first, the
answer is the commit with `index_pending: true` straight away, since the commit landed whatever happens next.

Other instances sync when they get the notice, within seconds. Notices are a shortcut: a lost one only means
waiting for the next poll.

## Broken documents

A file speki can't parse can't be read through speki, so no agent can have seen it. `write`, `edit` and
`move` on it answer `broken`. `delete` with its current version is allowed, so an agent can clear out a file
that was never meant to be a document.
