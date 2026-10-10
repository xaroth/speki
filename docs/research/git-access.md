# Reading and writing vault repositories

Researched 2026-10-09.

## Getting the files onto disk

Speki runs on ephemeral storage. It needs every vault's markdown locally to build the index, and it
needs to notice changes cheaply.

| Option | Good | Bad |
|---|---|---|
| Shallow clone, then fetch | One request per vault for a full copy, incremental updates, no REST rate limit | Needs a git implementation |
| Tarball API | One HTTP request | No incremental update. Redirect link expires after 5 minutes |
| Recursive tree + contents API | No git needed | 100,000 entries / 7 MB per tree call, one request per file |
| Compare API for changes | Simple | Capped at 300 files |

We clone. Two ways to run git from Go:

- The `git` binary: fastest, everything supported, but the container needs git, and we parse output.
- `github.com/go-git/go-git/v5` (v5.19.3): pure Go, supports shallow single-branch clones with
  filesystem storage. No protocol v2 and no LFS, neither of which we need.

We use go-git, in line with the pure-Go preference (the index needs cgo, but the image stays free of
extra tools). Speki never checks out a working tree. It reads blobs
straight from the object store, which also means a symlink can't point anywhere.

Updating: `fetch --depth=1` for the default branch, then diff the old and new root trees. Both trees are
local after the fetch, so this works on a shallow clone and costs no API calls.

Milestone 0 check 4 confirmed this: a depth-1 fetch on top of a shallow copy keeps the previous commit, so
the tree diff works. 100 fetch and diff rounds on a 2,000-file repository took 7.5 s and left one pack file
per fetch, which the re-clone threshold handles.

Credentials: an installation token (1 hour, renewed as needed) passed as basic auth with the user
`x-access-token`. It is never part of the URL, so it can't leak through an error string.

## Noticing changes

Per vault, on a timer (default 60 seconds), the poller runs `GET /repos/{owner}/{repo}/git/ref/heads/{branch}`
with `If-None-Match`. A 304 doesn't count against the rate limit. When the commit changed, the poller
fetches and re-indexes the changed files. Twenty vaults polled every minute is 1,200 mostly free
requests an hour.

Discovering vaults works the same way, every 5 minutes: list the app's installations with the app JWT,
list each installation's repositories, and check for `speki.yaml` in repositories that were pushed to since
the last check. A removed installation shows up as a 404 when speki asks for its token.

## Writing

Every write is a commit on the default branch, made with the user's token, so GitHub shows the user as the
author and checks their push access.

| API | Files per commit | Concurrency check | Signed | Push protection |
|---|---|---|---|---|
| Contents `PUT`/`DELETE` | One | Blob SHA of that file | Undocumented for user tokens | Yes, documented |
| Git Data (blobs, trees, commits, refs) | Any | Non-fast-forward ref update | Only if we sign | Undocumented |
| GraphQL `createCommitOnBranch` | Any, additions and deletions | `expectedHeadOid` | Yes, by GitHub | Undocumented |

`move` needs a delete and an add in one commit, which rules out the Contents API. We use
`createCommitOnBranch`: atomic, signed, with a branch-level check that turns a lost race into a clean error.

Not yet verified: whether push protection rejects a secret sent through `createCommitOnBranch`, and which
errors branch rulesets produce through it. If push protection doesn't apply, the Git Data API (blobs,
tree, commit, non-forced ref update) does the same job. Either way the choice sits in one method of `gh`.

Edits to a document check that the file's blob SHA still matches what the agent read. GitHub's branch
check alone would also reject concurrent changes elsewhere in the vault. Speki retries those against the
new head as long as the target file itself didn't change.

GitHub's secondary limit for content creation is 80 requests a minute and 500 an hour per user. Normal
agent use won't hit that. Speki reports a 403 with `retry-after` as a `rate_limited` error with the wait time.

## History

`history` calls `GET /repos/{owner}/{repo}/commits?path=...` with the user's token. Speki's clone is
shallow and has no history. Using the user's token means GitHub enforces access for this call too.
