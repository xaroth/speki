# Vaults

## Making a repository a vault

1. Install the speki GitHub App on the repository. When installing on an organisation, pick "Only select
   repositories" and select it.
2. Add a `speki.yaml` file to the root of the default branch.

Speki checks for new vaults every 5 minutes. A new vault becomes searchable once speki has cloned and
indexed it, usually within a minute after that.

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
```

| Field | Required | Used for |
|---|---|---|
| `title` | yes | Shown in `list` and `guide`. Keep it under 100 characters |
| `description` | yes | Tells agents what belongs in this vault, so they search and write in the right place. Keep it under 300 characters |
| `guidelines` | no | Writing rules for agents. Returned by `guide` and when listing the vault. Keep it under 2,000 characters |

Speki ignores unknown keys, so a newer `speki.yaml` doesn't break an older speki.

A `speki.yaml` that doesn't parse, misses a required field, goes over a length limit or is larger than 64
KiB is rejected. A new
vault with a bad file isn't added. An existing vault keeps running with its last good configuration. Either
way the problem is logged and shown on speki's status page, so an operator can see what's wrong.

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
