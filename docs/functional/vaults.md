# Vaults

## Making a repository a vault

1. Install the speki GitHub App on the repository. When installing on an organisation, pick "Only select
   repositories" and select it.
2. Add a `speki.yaml` file to the root of the default branch.

Speki checks for new vaults every 5 minutes (`sync.discovery_interval`). A new vault becomes searchable
once speki has cloned and indexed it, usually within a minute after that.

Both steps are needed. Installing the app on "all repositories" doesn't turn every repository into a vault.
Adding `speki.yaml` to a repository without the app does nothing either.

Speki only accepts installations owned by the accounts listed in its configuration. By default that is
the account that owns the GitHub App. See [running.md](running.md).

To retire a vault, remove `speki.yaml` or uninstall the app from the repository. Speki drops the vault at
the next discovery round.

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
```

| Field | Required | Used for |
|---|---|---|
| `title` | yes | Shown in `list` and `guide`. At most 100 characters |
| `description` | yes | Tells agents what belongs in this vault, so they search and write in the right place. At most 300 characters |
| `guidelines` | no | Writing rules for agents. Returned by `guide` and when listing the vault's root. At most 2,000 characters |
| `read_role` | no | The lowest GitHub role that sees the vault: `read` (the default), `triage`, `write`, `maintain` or `admin` |
| `readers` | no | Users and teams that may read the vault. Leave it out to let everyone with read access in |
| `writers` | no | Users and teams that may write to the vault. Leave it out to let every reader with push access write |
| `frontmatter.allow` | no | Frontmatter keys agents may set on top of `title`, `description`, `tags`, `aliases`, `status` and `related`. Leave it out to allow none. At most 50; `created` and `updated` can't be listed. Returned by `guide` |

Speki ignores unknown keys, so a newer `speki.yaml` doesn't break an older speki.

A `speki.yaml` that doesn't parse, misses a required field, goes over a length limit or is larger than
64 KiB is rejected. A new vault with a bad file isn't added. An existing vault keeps running with its last
good configuration. Either way the problem is logged and shown on speki's status page, so an operator can
see what's wrong.

## Limiting who reads and writes a vault

By default anyone with read access to the repository can read the vault, and anyone with push access can
write to it. In an organisation whose base permission is Read, that's every member. Three keys narrow
this. Use whichever fits; they combine.

```yaml
read_role: triage
readers: [ours/knowledge, ours/fc-leads, alice]
writers: ["@ours/knowledge"]
```

| To | The user needs |
|---|---|
| read | at least `read_role` on the repository, and to be in `readers` |
| write | all of the above, push access to the repository, and to be in `writers` |

`read_role` is the lowest GitHub role that sees the vault: `read` (the default), `triage`, `write`,
`maintain` or `admin`. Triage keeps people read-only on GitHub except for issues and pull requests, so
granting it, directly or through a team, is a way to pick who sees a vault without giving them write
access.

`readers` and `writers` list GitHub logins (`alice`) or teams as `org/team-slug`. A leading `@` is allowed,
as in CODEOWNERS, but YAML needs quotes around it. Teams must belong to the organisation that owns the
vault. Members of a child team count as members of its parent. Checking teams uses the app's "Members:
read" organisation permission, which speki already needs.

A key that's left out doesn't narrow anything. An empty list is rejected, so a typo can't silently open or
close a vault. Users are matched by login. A renamed account needs its new login listed.

Anyone who can push to the default branch can change these keys. When that matters, protect `speki.yaml`
with a push ruleset that restricts its path. Speki only writes `.md` files, so the ruleset doesn't block it.

## What speki reads from a vault

Every `.md` file on the default branch, except:

- paths with any segment starting with `.` (`.github/`, `.obsidian/`, `.trash/`)
- symlinks and submodules
- files tracked by git LFS
- files over 1 MiB

Speki is built and tested for vaults of up to 50,000 documents and 500 MB of markdown. Larger vaults
still work: speki indexes and serves them, but logs a warning, marks them on the status page, and they
take longer to index and search. Splitting a vault that keeps growing past that size is usually better
for everyone. Other files (images, PDFs, code) can sit in the repository but speki doesn't look at them.

## Branch rules

Speki writes directly to the default branch. If the branch requires pull requests or status checks, GitHub
rejects speki's commits, and agents get an error saying the vault doesn't accept direct writes. Reading
and searching still work.

Rules that only restrict force pushes or deletion are fine.
