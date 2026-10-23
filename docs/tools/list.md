# list

Lists the user's vaults, the vaults that cover a code repository, or the folders and documents in a vault.
Agents use it to browse when they don't know what to search for, and to find the vaults about the
repository they're working in.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | |
| `repository` | git remote URL, `host/owner/repo`, or `owner/repo` on github.com | |
| `path` | folder in the vault | the root |
| `recursive` | bool | false |
| `cursor` | the cursor the previous call ended with | |

All inputs are optional. Which ones are given picks one of three modes. Passing both `vault` and
`repository` is `invalid`: `repository` filters the list of vaults, so it has no meaning inside one.

## Without `vault`: the user's vaults

```text
acme/kb
Title: Infrastructure knowledge
Description: How our services are deployed and operated.
Repositories: github.com/acme/infra, github.com/acme/svc-*
Access: read and write. Documents: 412

acme/new
Title: New team notes
Not available: still building its index; try again shortly
```

The same vaults as [`guide`](guide.md), sorted by name: per vault its name, then the same title,
description, repositories and access or "Not available" lines, then a blank line. Frontmatter keys and
guidelines are left out. When no vault is served yet, it starts with "No vault is ready yet. Try again in
a minute."

## With `repository`: the vaults that cover it

Only the vaults whose `repositories` cover the given repository, in the same form, unserved ones included
with their "Not available" line. No match isn't an error: the text says "None of your vaults covers
<host/owner/repo>."

The repository is normalized the same way `speki.yaml` entries are ([vaults.md](../vaults.md#repositories)):
scheme, user, port and `.git` are dropped, `ssh.github.com` becomes `github.com`, and ASCII letters are
lowercased; a query string or fragment is dropped, and a local path is refused with a hint to pass the
remote instead. So an agent can pass `git remote get-url origin` as is. Error messages never quote the input,
because an HTTPS remote can carry a token.

## With `vault`: folders and documents

```text
runbooks/
services/
<results boundary="9f3c1a7e5b2d8c40">
README.md  About this vault  updated 2026-10-09
  What this vault holds and how it's organized.
</results boundary="9f3c1a7e5b2d8c40">
<guidelines boundary="9f3c1a7e5b2d8c40">
One page per service. ...
</guidelines boundary="9f3c1a7e5b2d8c40">
```

Folders come first, one per line. Documents go in one `results` fence: per document its path, title,
status in brackets when it isn't `active` and the day it was last updated, then its description on an
indented line when it has one. There's no version: `read` gives it, and an agent reads a document before
changing it.

- Entries are sorted by path, at most 200 per call, folders included. Without `recursive`, each subfolder
  is one entry ending in `/`. With it, every document under `path` is listed and there are no folder
  entries.
- When more entries follow, the documents are followed by "More entries follow: call list again with
  cursor "..."." The agent passes that as `cursor` with the same `vault` and `path`. Paging is by path
  rather than by offset, so the cursor stays valid when documents are added or removed in between.
- The vault's guidelines come last, in a `guidelines` fence, when listing the vault's root without a
  cursor, so an agent that starts browsing a vault sees its rules.
- A document whose file can't be parsed is listed as `[broken]`, with its path as title.
- Titles are cut to 200 bytes and descriptions to 500.
- With nothing to show, the text says "This vault has no documents yet."

## Errors

| Code | When |
|---|---|
| `invalid` | Both `vault` and `repository`; a malformed `repository`; a cursor from another listing |
| `not_found` | No such vault the user can read; a folder other than the root with nothing under it |
| `unavailable` | The vault is building, failed or gone |
