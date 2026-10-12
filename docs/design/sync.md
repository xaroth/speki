# Vaults, local copies and writes

Packages: `vault` (discovery), `gitsync` (git), `library` (keeping copies current), `gh` (GitHub API),
`knowledge` (the write path), `store` (change notices).

## Discovery

`vault.Registry` holds the current set of vaults. Its loop runs at startup and then every
`sync.discovery_interval` (default 5 minutes):

1. List the app's installations with the app JWT. Keep those whose account ID is in
   `github.allowed_accounts`.
2. For each installation, get an installation token and list its repositories.
3. For each repository whose `pushed_at` changed since the last round, or that's new, fetch `speki.yaml`
   from the default branch. Repositories without one are remembered as "not a vault" until they're pushed
   to again. With "all repositories" installs on large organisations, this keeps discovery from fetching
   hundreds of files every round.
4. Parse and validate `speki.yaml` (see [vaults.md](../functional/vaults.md) for the limits).
5. Update the registry and tell subscribers (`library`) what appeared, changed or disappeared.

```go
// Package vault
type Vault struct {
	RepoID         int64  // the key
	Name           string // "owner/repo", what people and agents use
	InstallationID int64
	DefaultBranch  string
	Config         Config // parsed speki.yaml
}
```

A vault is only removed on a definite answer: GitHub says the installation or repository is gone, the
repository no longer has `speki.yaml`, the account isn't allowed anymore, or the installation is
suspended. A removed vault's copy and index are deleted, so unsuspending an installation means cloning and
indexing its vaults again. A failed request changes nothing. A `speki.yaml` that stops parsing keeps the
vault running on its last good configuration, and the problem is logged and shown on the status page
(see [operations.md](operations.md)). A new vault with a bad `speki.yaml` is not added.

## Git copies

`gitsync.Mirror` is a shallow copy of one repository: a bare repository under
`<data_dir>/<repo id>/git`, cloned with go-git, depth 1, single branch, no tags, no working tree.

```go
// Package gitsync
func Open(ctx context.Context, dir string, src Source) (*Mirror, error) // clones when dir is empty or broken
func (m *Mirror) SetSource(url string, auth TokenSource)              // where Fetch fetches from
func (m *Mirror) Fetch(ctx context.Context, progress io.Writer) (head string, err error)
func (m *Mirror) Diff(from, to string) ([]File, error)   // changed eligible files; empty Blob when deleted
func (m *Mirror) Files(at string) ([]File, error)        // every eligible file, for full builds
func (m *Mirror) Blob(sha string) ([]byte, error)
func (m *Mirror) Paths(at string) ([]string, error)      // every file, any type
```

Every fetch goes to the URL and credentials the mirror was last given, never to the remote stored in the
copy when it was cloned. Before each sync, `library` sets them from the vault's current name and
installation. A vault is its repository ID, so after a rename or a transfer to another installation the
copy fetches from the new name with the new installation's token. A new repository that takes the old
name is never fetched.

Discovery learns a new name only every few minutes, while polls and syncs go by name every minute. So
before each sync, `library` asks GitHub which repository the name points to now (`GET /repos/{owner}/{repo}`,
one request per sync, not per poll). When it isn't the vault's repository ID, the sync is refused and
recorded as a failure, and the vault keeps serving its last good index until discovery reports the new
name.

Eligible files: `.md`, regular file modes `100644` and `100755`, no path segment starting with `.`, at
most 1 MiB, not an LFS pointer. Symlinks and submodules are never eligible. A rename shows up as a delete
plus an add. Paths are reported as the raw bytes git has.

`createCommitOnBranch` has no file modes, so a change writes a file as `100644`: an executable `.md` file
loses its executable bit when speki edits or moves it.

Every walk of a tree, and `Diff`, looks each folder up itself (go-git's `DiffTree` treats a missing
folder as empty, and refuses folder names with control characters). So a missing object is an error
(`gitsync.ErrBroken`) rather than a quiet gap, and a path with a control character, which git allows, is
not eligible. A copy that turns out broken while in use is cloned again, next to the old one, and
the index rebuilt from it; a quick sync for a write leaves that to the poller. A `read` that finds a blob
missing marks the copy, and the next poll does the same.

`Paths` sees every file, eligible or not. The write path uses it to make sure it doesn't overwrite
something the index doesn't know about.

`TokenSource` is the one-method interface `gitsync` declares for getting an installation token. The
token goes in as basic auth with user `x-access-token`, never in the URL. The remote URL is built from
configuration (`https://github.com/<owner>/<repo>.git` by default), which is how tests point it at a local
bare repository.

A mirror counts as broken when walking the tree at its head fails. A broken mirror is deleted and cloned
again. Depth-1 fetches leave a new pack file behind each time. When a mirror has more than 50 pack files,
`library` clones it fresh next to the old copy and swaps it in, at most once an hour. That's simpler and
safer than repacking a shallow repository. The fresh clone runs under the vault's sync lock, either at the
end of a full sync or as a background job when the poller finds the vault current.

## Keeping copies current

`library.Library` owns one `library.Vault` per registered vault:

```go
type Vault struct {
	mu     sync.Mutex // guards the vault's details and status
	info   vault.Vault
	status Status

	syncMu syncLock     // one open, sync, fresh clone or removal at a time
	gitMu  sync.RWMutex // fetches and swapping in a fresh clone exclude reads of the git copy
	mirror *gitsync.Mirror
	index  *index.Index
}
```

`syncLock` is a mutex a caller can stop waiting for. Reads of the git copy (`Blob`, `Paths`) take `gitMu`
for reading, so a `read` never waits for a whole sync, only for a fetch. Locks are taken in the order:
`library`'s opening slot (the limit of four clones or builds at a time), `syncMu`, `gitMu`, `mu`. The write
lock is not here: `knowledge` keeps one per vault, and takes it before anything in `library`.

A sync:

1. `mirror.Fetch`.
2. Read the commit the index is at (`index.Commit()`), not the mirror's previous head. If the fetch
   succeeded but the last apply failed, the next sync still applies the difference.
3. `mirror.Diff(indexCommit, head)`. If the index's commit isn't in the mirror anymore (after a fresh
   clone), build the index from scratch instead.
4. `index.Apply(head, changes)`.

Anything that wants fresher data calls `Sync` on that vault: the poller, the write path, a change notice
from another instance. A caller waits for a running sync to finish and then checks whether another is
needed. A vault is never synced twice at once.

## Polling

Every `sync.poll_interval` (default 60 seconds), `library` checks every vault, 4 at a time:

1. `GET /repos/{owner}/{repo}/git/ref/heads/{branch}` with `If-None-Match`. A 304 means nothing changed and
   costs no rate limit.
2. If the head differs from the index's commit, fetch and apply the changes, in the round.

Anything slower runs in the background, four vaults at a time, so one vault can't hold up the others: a
full rebuild, a fresh clone, cloning a vault that failed to open, and switching to a new default branch
(cloned next to the old copy while the old index keeps serving; documents whose content isn't on the new
branch can't be read until the rebuild finishes). A vault busy with one of those is
skipped by the round and checked again at the next.

A vault whose head check or sync fails keeps serving its last good index, shows the error on the status
page, and is retried with backoff that grows from two minutes to 30, across restarts of its clone too.

On startup, `library` restores only vaults whose git copy is usable without the network. The rest are
cloned again in the background once discovery finds them. An index file that can't be opened for a
reason that may pass (a cancelled start, too many open files) is kept; only a corrupt one, one of another
schema or one without a recorded commit is discarded.

## Writes

`knowledge` prepares the change (see [architecture.md](architecture.md) for the steps) and commits through a
small interface it declares:

```go
// Package knowledge
type gitHub interface {
	Commit(ctx context.Context, token string, req gh.CommitRequest) (sha string, err error)
	// ... and Commits, for history
}

// Package gh
type FileChange struct {
	Path    string
	Content []byte // nil deletes the path
}

type CommitRequest struct {
	Repo         string // owner/repo
	Branch       string
	ExpectedHead string
	Message      string
	Changes      []FileChange
}
```

`gh.Client.Commit` uses GraphQL `createCommitOnBranch`: several changes in one commit (`move` is a delete
plus an add), checked against `expectedHeadOid`, signed by GitHub. Push protection and branch rulesets
apply to it (see [git-access.md](../research/git-access.md)).

Before committing, `knowledge` checks the target path against every path in the mirror's tree at the
expected head (`Paths`, plus the folders they imply): a path that exists (as any kind of entry), or that
differs only in letter case from an existing file or folder, is refused for creation, and so is a file
that differs only in Unicode normalisation. A folder that differs only in normalisation takes git's
spelling (see `CheckPath` in [document.md](document.md)). When changing an existing file, speki writes
back to the exact path bytes git has.

Commit messages: the first line of the agent's message, or a default, cut to at most 200 bytes. A line that
looks like a `Speki-Client:` trailer is replaced by the default. Speki then appends its own trailers: the
client name (one line, printable characters, at most 64) and `Speki-Request:` with a random ID for that
attempt. `history` reads only the last `Speki-Client` trailer of a message.

`gh` turns GitHub's answers into errors `knowledge` can act on:

| GitHub says | `gh` returns | Tool sees |
|---|---|---|
| expected head doesn't match | `gh.ErrHeadMoved` | retried up to three times, then `conflict` |
| push protection found a secret | `*gh.SecretError{Detail}` | `secret_detected`, with GitHub's message |
| rule violation, protected branch | `gh.ErrBranchRules` | `vault_read_only` |
| 403 or 404, "not accessible", or `FORBIDDEN`/`NOT_FOUND` for any other reason | `gh.ErrForbidden` | `no_write_access` |
| secondary rate limit | `*gh.RateLimitError{RetryAfter}` | `rate_limited` |

GitHub documents the GraphQL error types but not their messages. Push protection and rulesets both come
back as type `FORBIDDEN`, the same type as a missing permission, so `gh` matches the messages first and
only then falls back to the type. The responses recorded from github.com are in `gh/testdata/`. A refused
commit is logged with GitHub's message, since the agent only gets the code.

When the agent's version doesn't match the index, `knowledge` syncs once before answering `conflict`: the
agent may have read a version GitHub has and the index doesn't yet. One write runs at a time per vault on
an instance; between instances, the expected head keeps commits from overwriting each other.

When GitHub's answer to a commit is lost (a dropped connection, a timeout, a 5xx), the commit may have
landed anyway. Every commit carries a random `Speki-Request:` trailer, so before reporting a failure,
`knowledge` asks GitHub, as the user, for the latest commits that touched the path and looks for it. If
it's there, the change is reported as a success with that commit, so an agent doesn't retry an `edit`
that would then apply twice. GitHub's own timeout answers ("This may be the result of a timeout", with
HTTP 200) count as a lost answer, and the lookup is tried a second time after two seconds, in case GitHub
is still finishing the commit. Those are recognised by their message only. Any other GraphQL error counts
as a definite refusal and is never looked for, so a new kind of GitHub error that hides a landed commit
would let a retried `edit` apply twice. An agent that hung up before speki could check can still retry;
passing `version` to `edit` turns such a retry into a `conflict`.

Syncs started for a write, before or after the commit, wait at most 10 seconds, and only apply changes.
A full rebuild or a fresh clone is left to the poller: after a commit, the write answers with
`index_pending`; before one, it answers `unavailable` ("rebuilding its index"). The sync after a commit
carries on if the client goes away, for at most 10 seconds.

Writes add a pack file each without the poller needing to sync, so the poller also compacts a copy whose
head check found nothing new. If a commit lands during that fresh clone, the new copy lacks the index's
commit, and the index could only catch up by a full rebuild; so that copy is thrown away, and the next
round tries again.

After a successful commit, `knowledge`:

1. computes the new version: the git blob hash of the content it sent
2. asks the vault to sync, and waits until a sync that started after the commit has finished, at most 10
   seconds
3. publishes `{repo id, commit}` on the store's change channel (a no-op with the in-memory store)
4. returns the version, the commit SHA, and `index_pending: true` if step 2 timed out

## Change notices between instances

With Redis configured, each instance subscribes to `speki:vault-head`. A notice for a vault whose index is
at a different commit triggers a sync of that vault. Notices are a shortcut. A lost notice only means
waiting for the next poll.

With the in-memory store there is only one instance, and nothing to tell.

## History

`knowledge.History` calls `GET /repos/{owner}/{repo}/commits` with `path`, `sha=<branch>` and
`per_page=limit`, using the user's token from the `Grant`.

## Rate limit budget

Installation tokens get 5,000 to 12,500 requests an hour. Speki's own background use, at 20 vaults:

| What | Requests per hour |
|---|---|
| Polling every minute | about 1,200, nearly all 304 and free |
| Discovery every 5 minutes | about 12 × (1 + installations), plus `speki.yaml` fetches only for pushed repositories |
| Repository ID checks | one per sync, so only for vaults that changed |
| Installation tokens | a few |

User tokens carry the per-user calls: access checks (at most once every 5 minutes per token while active, shared by every instance),
commits and history.

`gh` records `x-ratelimit-remaining` per token kind as a metric and logs a warning below 10%, at most
once every 10 minutes per kind and installation. Each request is counted once, by the credential it
carries: an installation's calls go through the app's transport, which only counts the ones signed with
the app's JWT.
