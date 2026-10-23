# Vaults

A vault is a GitHub repository that holds knowledge. This page covers how a repository becomes one, what
its `speki.yaml` says, who may read and write it, and how speki keeps its copy of it current.

## Making a repository a vault

1. Install the speki GitHub App on the repository. On an organization, pick "Only select repositories" and
   select it.
2. Add a `speki.yaml` file to the root of the default branch.

Both steps are needed. Installing the app on "all repositories" doesn't turn every repository into a vault,
and a `speki.yaml` in a repository without the app does nothing. A vault is something a team opts into on
purpose.

Speki only counts installations owned by the accounts in its configuration, by default the account that
owns the GitHub App ([running.md](running.md#configuration)). Accounts are matched by numeric ID, because
names can be given up and claimed by someone else. So even a public app can't be used by a stranger to push
content into speki.

Speki looks for new vaults every 5 minutes (`sync.discovery_interval`). A new vault is served once speki has
cloned and indexed it, usually within a minute after that.

To retire a vault, remove `speki.yaml` or uninstall the app from the repository. Speki drops the vault at the
next discovery round and deletes its local copy.

## speki.yaml

```yaml
title: Infrastructure knowledge
description: >
  How our services are deployed and operated: hosts, pipelines, runbooks and the reasons behind them.
guidelines: |
  Put one document per service under services/, named after the service.
  Runbooks go under runbooks/ and start with a "When to use this" section.
  Mark anything that describes the old Nomad setup as deprecated instead of deleting it.
frontmatter:
  allow: [owner, review_by]
repositories:
  - acme/speki-infra
  - github.com/acme/web-*
  - gitlab.com/team/sub/deploy
```

| Field | Required | Used for |
|---|---|---|
| `title` | yes | Shown in `list` and `guide`. At most 100 characters |
| `description` | yes | Tells agents what belongs in this vault, so they search and write in the right place. At most 300 characters |
| `guidelines` | no | Writing rules for agents, returned by `guide` and when listing the vault's root. At most 2,000 characters |
| `read_role` | no | The lowest GitHub role that sees the vault: `read` (the default), `triage`, `write`, `maintain` or `admin` |
| `readers` | no | Users and teams that may read the vault. Left out, everyone with read access may |
| `writers` | no | Users and teams that may write to the vault. Left out, every reader with push access may |
| `frontmatter.allow` | no | Frontmatter keys agents may set on top of the core fields. At most 50; `created` and `updated` can't be listed. Returned by `guide` |
| `repositories` | no | The code repositories the vault's knowledge is about. At most 50. See [Repositories](#repositories) |

The length limits keep `guide` readable when a user has many vaults. Speki ignores unknown keys, so a
newer `speki.yaml` doesn't break an older speki.

A `speki.yaml` that doesn't parse, misses a required field, goes over a limit or is larger than 64 KiB is
rejected. A new vault with a bad file isn't added. An existing vault keeps running on its last good
configuration, so a typo doesn't take a vault away from everyone. After a restart, the old configuration
serves until the first discovery round in which every request to GitHub succeeds; that round drops a vault
whose file is still bad and deletes its local copy, and fixing the file means cloning and indexing it again.
Either way the problem is logged and shown on the status page ([operating.md](operating.md#status-page)).
Speki reads the file again at the first discovery round after a push to the repository or a change of its
default branch, and at startup.

The guidelines are written by anyone who can push to the repository. Agents get them inside a fence, like
document text, and are told to treat them as information
([security.md](security.md#prompt-injection-stored-in-documents)).

## Repositories

`repositories` tells agents which code repositories a vault covers. An agent working in a checkout passes
its git remotes to `list` or `search` as `repositories`, and finds the knowledge about that code without
knowing vault names. A vault can cover several repositories, and several vaults can cover the same one.
List the upstream repository, not forks: a fork's checkout has the upstream as a remote, and any remote
matching is enough.

Each entry is `host/owner/repo`, or `owner/repo` for github.com. A git remote URL works too
(`https://github.com/acme/speki.git`, `git@gitlab.com:team/sub/deploy.git`): speki drops the scheme,
user, port and `.git`, maps `ssh.github.com` to `github.com`, and lowercases ASCII letters. `owner` can
hold GitLab subgroups, but a github.com entry is always exactly `owner/repo`.

A `*` in the repository name covers every repository of that owner whose name matches:
`acme/web-*` covers `acme/web-core` but not `acme/web`. `*` can't be used in the host or
owner, and covers one level only: `gitlab.com/team/*` doesn't cover `gitlab.com/team/sub/deploy`.

Speki doesn't check that the repositories exist or who can see them. Entries are shown to everyone who can
read the vault, so don't list a private repository's name in a vault with a wider audience.

## Limiting who reads and writes a vault

By default, anyone with read access to the repository can read the vault, and anyone with push access can
write to it. In an organization whose base permission is Read, that's every member. Three keys narrow this,
and they combine:

```yaml
read_role: triage
readers: [ours/knowledge, ours/fc-leads, alice]
writers: ["@ours/knowledge"]
```

| To | The user needs |
|---|---|
| read | at least `read_role` on the repository, and to be in `readers` |
| write | all of the above, push access to the repository, and to be in `writers` |

These keys only narrow what GitHub allows, never widen it. Triage keeps people read-only on GitHub except
for issues and pull requests, so granting it, directly or through a team, picks who sees a vault without
giving them write access.

`readers` and `writers` list GitHub logins (`alice`) or teams as `org/team-slug`. A leading `@` is allowed,
as in CODEOWNERS, but YAML needs quotes around it. Teams must belong to the organization that owns the
vault, since that's the organization speki can ask GitHub about. Members of a child team count as members
of its parent. Users are matched by login, so a renamed account needs its new login listed. Speki learns
the new login when the user's client next refreshes its access, within an hour of the rename. Until then the
old login still matches.

A key that's left out doesn't narrow anything. An empty list is rejected, so a typo can't silently open or
close a vault.

Anyone who can push to the default branch can change these keys. When that matters, protect `speki.yaml`
with a push ruleset that restricts its path. Speki only writes `.md` files, so the ruleset doesn't block it.

## What speki reads from a vault

Every file on the default branch whose name ends in `.md`, except:

- paths with any segment starting with `.` (`.github/`, `.obsidian/`, `.trash/`)
- symlinks and submodules, which could point outside the vault
- files tracked by git LFS, since a clone only has the pointer
- files over 1 MiB
- paths with a control character, which git allows

Other files (images, PDFs, code) can sit in the repository, but speki doesn't look at them. Binary files
would need upload limits, content type checks, a way to return images to models, and decisions about LFS,
for content agents mostly write as text.

Speki is built and tested for vaults of up to 50,000 documents and 500 MB of markdown. Larger vaults still
work: speki indexes and serves them, but logs a warning, marks them on the status page, and they take longer
to index and search.

## Branch rules

Speki writes directly to the default branch and never opens pull requests. If the branch requires pull
requests or status checks, GitHub rejects speki's commits, and agents get `vault_read_only`. Reading and
searching still work. Rules that only restrict force pushes or deletion are fine.

## How speki keeps a copy

Speki serves reads from its own copy of each vault, never by asking GitHub for content. The copy is two
things, both rebuilt from GitHub when missing: a shallow git clone of the default branch, and a SQLite
search index built from it.

### Discovery

Every discovery round lists the app's installations, keeps those of allowed accounts, lists each one's
repositories, and fetches `speki.yaml` only from repositories pushed to, or whose default branch changed,
since the last round. With "all repositories" installs on large organizations, that keeps discovery from
fetching hundreds of files every 5 minutes. With Redis, the installation and repository lists are shared
between instances, like branch heads below. Each instance still fetches `speki.yaml` itself, since an older
shared copy would hide a new one until the next push.

A vault is only removed on a definite answer: GitHub says the installation or repository is gone, the
repository no longer has `speki.yaml`, the account isn't allowed anymore, or the installation is
suspended. A failed request changes nothing. A suspended installation's vaults are removed with their
copies, so unsuspending means cloning and indexing them again.

### The git copy

The clone is shallow (depth 1), single branch, without tags, LFS or submodules, and without a working
tree. Speki reads files straight from git's object store, so a symlink in a vault can't point anywhere on
disk.

Speki clones rather than using GitHub's file APIs: one request gives a full copy, updates are incremental,
the diff between two commits is computed locally, and none of it counts against the REST rate limit. It
uses go-git, a git implementation in Go, so the container image needs no git binary.

Each update fetches the new head at depth 1 and diffs the old and new trees locally. Every fetch leaves a
pack file behind. When a copy has more than 50, speki clones it fresh next to the old one and swaps it in, at
most once an hour, which is simpler and safer than repacking a shallow repository. This also happens when the
poller finds the vault current, since an instance's own writes add packs without the poller ever syncing. If
a commit lands during the clone, the clone is thrown away and tried again 10 minutes later. If the swap fails,
the old copy goes back in place and keeps serving; if it can't go back, the vault is marked broken and its next
full sync clones it again.

A vault is identified by its repository ID, not its name. After a rename or a transfer, the copy fetches
from the new name with the new installation's credentials, and keeps its files and index. Before each
sync, speki checks that the name still points to the vault's repository ID, so a new repository created
under an old name is never fetched.

### Polling

Every minute (`sync.poll_interval`), speki asks GitHub for each vault's branch head with a conditional
request. An unchanged head costs no rate limit. When the head moved, speki fetches, diffs and applies the
changed files to the index. Twenty vaults polled every minute are about 1,200 requests an hour, nearly all
free.

With Redis, instances share these answers. The first instance due to ask takes a 30-second lock in Redis,
asks GitHub with the ETag of the shared answer, and stores the result. The others use it until it's 90% of
the poll interval old, so N instances cost about as much of the rate limit as one. If Redis fails, each
instance asks GitHub itself. Each instance still fetches its own git copy.

Each poll round checks four vaults at a time. Slower work runs in the background, outside the round, so one
vault can't hold up the others: a full rebuild, opening a vault that failed, or switching to a new default
branch (cloned next to the old copy while the old index keeps serving). At most four of those run at once; a
fresh clone that only compacts a copy doesn't count against them. A clone or build that makes no progress for
5 minutes is canceled and retried later. Large vaults that keep making progress are left to finish.

A vault whose update fails keeps serving its last good index and shows the error on the status page. It's
retried with backoff from 2 minutes up to 30. A failure from SQLite running out of memory, usually caused by
other work such as a heavy query, is retried after a minute instead.

On startup, speki serves every vault whose git copy and index on disk are usable, opening up to four at once.
It checks each copy by walking its tree once, without the network. A copy it can't use is left in place and
cloned again in the background once discovery finds the vault. An index file that can't be opened for a
reason that may pass (a canceled start, too many open files) is kept; only a corrupt one, one of another
schema, or one without a recorded commit is discarded.

Writes through speki and, with Redis, change notices from other instances update the copy right away
([changes.md](changes.md#after-the-commit)). Polling is what guarantees changes made outside speki arrive.

### The index

Each vault has one SQLite file holding its documents, tags, links, headings and full-text indexes
([search.md](search.md)). An update applies the changed files in one transaction, so readers see the old
state or the new one, never a mix. A full rebuild writes a new file next to the live one and switches over
when it's complete; the old file is deleted once nothing reads it. A file is never replaced while open,
which is one of the ways SQLite databases get corrupted.

The index has a schema version. An index with another version is rebuilt from git. There are no
migrations, because rebuilding is always possible.

A file that can't be parsed (not UTF-8, or frontmatter over 64 KiB) is indexed as broken instead of
failing the update. One bad file must not stop a vault from updating: a document deleted because it held a
secret would otherwise stay searchable.

### Vault states

| State | Meaning |
|---|---|
| building | Being cloned and indexed for the first time. Not served yet |
| serving | Served from its copy |
| failed | Couldn't be opened. Retried with backoff; the status page has the error |

To agents, a readable vault that isn't served shows up with its state, and also as `gone` when it was
removed or reopened while a request ran ([tools/overview.md](tools/overview.md#vaults-speki-cant-serve)).
