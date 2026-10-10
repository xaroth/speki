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
repository no longer has `speki.yaml`, or the account isn't allowed anymore. A failed request changes
nothing. A `speki.yaml` that stops parsing keeps the vault running on its last good configuration, and the
problem is logged and shown on the status page (see [operations.md](operations.md)). A new vault with a
bad `speki.yaml` is not added.

## Git copies

`gitsync.Mirror` is a shallow copy of one repository: a bare repository under
`<data_dir>/<repo id>/git`, cloned with go-git, depth 1, single branch, no tags, no working tree.

```go
// Package gitsync
func Open(dir, remote string, auth TokenSource) (*Mirror, error) // clones when dir is empty
func (m *Mirror) Fetch(ctx context.Context) (head plumbing.Hash, err error)
func (m *Mirror) Diff(from, to plumbing.Hash) ([]Change, error) // changed eligible files between two commits
func (m *Mirror) Files(at plumbing.Hash) iter.Seq2[File, error] // every eligible file, for full builds
func (m *Mirror) Blob(sha plumbing.Hash) ([]byte, error)
func (m *Mirror) Lookup(at plumbing.Hash, path string) (Entry, bool, error) // any path, any type
```

Eligible files: `.md`, regular file modes `100644` and `100755`, no path segment starting with `.`, at
most 1 MiB, not an LFS pointer. Symlinks and submodules are never eligible. A rename shows up as a delete
plus an add. Paths are reported as the raw bytes git has.

`Lookup` sees every entry, eligible or not. The write path uses it to make sure it doesn't overwrite
something the index doesn't know about.

`TokenSource` is the one-method interface `gitsync` declares for getting an installation token. The
token goes in as basic auth with user `x-access-token`, never in the URL. The remote URL is built from
configuration (`https://github.com/<owner>/<repo>.git` by default), which is how tests point it at a local
bare repository.

A mirror counts as broken when walking the tree at its head fails. A broken mirror is deleted and cloned
again. Depth-1 fetches leave a new pack file behind each time. When a mirror has more than 50 pack files,
`library` clones it fresh in the background and swaps it in. That's simpler and safer than repacking a
shallow repository.

## Keeping copies current

`library.Library` owns one `library.Vault` per registered vault:

```go
type Vault struct {
	vault  vault.Vault
	mirror *gitsync.Mirror
	index  *index.Index

	syncMu  sync.Mutex // one sync at a time
	writeMu sync.Mutex // one write at a time, on this instance
}
```

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
2. If the head differs from the index's commit, sync.

A vault that fails to sync keeps serving its last good index and is retried with backoff. Its error shows
on the status page.

## Writes

`knowledge` prepares the change (see [architecture.md](architecture.md) for the steps) and commits through a
small interface it declares:

```go
// Package knowledge
type committer interface {
	Commit(ctx context.Context, token string, c gh.Commit) (sha string, err error)
}

// Package gh
type FileChange struct {
	Path    string
	Content []byte // nil deletes the path
}

type Commit struct {
	Repo         string // owner/repo
	Branch       string
	ExpectedHead string
	Message      string
	Changes      []FileChange
}
```

`gh.Client.Commit` uses GraphQL `createCommitOnBranch`: several changes in one commit (`move` is a delete
plus an add), checked against `expectedHeadOid`, signed by GitHub. Whether GitHub's push protection
applies to this mutation isn't documented, and is checked before anything is built on it (see
[PLAN.md](../PLAN.md)). If it doesn't, `Commit` uses the Git Data API (blobs, tree, commit, non-forced ref
update) for every write instead. Nothing outside
`gh` changes either way.

Before committing, `knowledge` checks the target path against the mirror at the expected head with
`Lookup`: a path that exists (as any kind of entry), or that differs only in letter case or Unicode
normalisation from an existing file or folder, is refused for creation. When changing an existing file,
speki writes back to the exact path bytes git has.

Commit messages: the agent's message, or a default, is cut to one line of at most 200 characters. Lines that
look like a `Speki-Client:` trailer are removed. Speki then appends its own trailer with the client name
(one line, printable characters, at most 64). `history` reads only the last `Speki-Client` trailer of a
message.

`gh` turns GitHub's answers into errors `knowledge` can act on:

| GitHub says | `gh` returns | Tool sees |
|---|---|---|
| expected head doesn't match | `gh.ErrHeadMoved` | retried up to three times, then `conflict` |
| rule violation, protected branch | `gh.ErrBranchRules` | `vault_read_only` |
| push protection found a secret | `gh.ErrSecretDetected` | `secret_detected`, with GitHub's description of the secret type |
| 403 or 404 on the repository | `gh.ErrForbidden` | `no_write_access` |
| secondary rate limit | `gh.RateLimitError{RetryAfter}` | `rate_limited` |

After a successful commit, `knowledge`:

1. computes the new version: the git blob hash of the content it sent
2. asks the vault to sync, and waits until a sync that started after the commit has finished, at most 10
   seconds
3. publishes `{repo id, commit}` on the store's change channel
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
| Installation tokens | a few |

User tokens carry the per-user calls: access checks (at most once a minute while active, mostly 304),
commits and history.

`gh` records `x-ratelimit-remaining` per token kind as a metric and logs a warning below 10%.
