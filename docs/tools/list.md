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
| `cursor` | `next_cursor` from the previous call | |

All inputs are optional. Which ones are given picks one of three modes. Passing both `vault` and
`repository` is `invalid`: `repository` filters the list of vaults, so it has no meaning inside one.

## Without `vault`: the user's vaults

```json
{ "vaults": [
  { "vault": "acme/kb", "title": "Infrastructure knowledge",
    "description": "How our services are deployed and operated...",
    "repositories": ["github.com/acme/infra", "github.com/acme/svc-*"],
    "can_write": true, "documents": 412 },
  { "vault": "acme/new", "title": "New team notes", "description": "...",
    "can_write": false, "documents": 0, "state": "building" }
] }
```

The same entries as [`guide`](guide.md), sorted by name, without guidelines or frontmatter keys. Vaults
speki can't serve right now carry a `state`.

## With `repository`: the vaults that cover it

Only the vaults whose `repositories` cover the given repository, in the same form, unserved ones included
with their `state`. No match isn't an error: the `vaults` key is left out, and the text says no vault covers
the repository.

The repository is normalized the same way `speki.yaml` entries are ([vaults.md](../vaults.md#repositories)):
scheme, user, port and `.git` are dropped, `ssh.github.com` becomes `github.com`, and ASCII letters are
lowercased. So an agent can pass `git remote get-url origin` as is. Error messages never quote the input,
because an HTTPS remote can carry a token.

## With `vault`: folders and documents

```json
{ "folders": ["runbooks/", "services/"],
  "documents": [
    { "path": "README.md", "title": "About this vault", "description": "...",
      "status": "active", "updated": "2026-10-09T14:02:11Z",
      "version": "3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1" }
  ],
  "guidelines": "Put one document per service under services/ ...",
  "next_cursor": "services/esi-proxy.md" }
```

- Entries are sorted by path, at most 200 per call, folders included. Without `recursive`, each subfolder
  is one entry ending in `/`. With it, every document under `path` is listed and there are no folder
  entries.
- `next_cursor` is set when more entries follow. Pass it as `cursor` with the same `vault` and `path`.
  Paging is by path rather than by offset, so the cursor stays valid when documents are added or removed in
  between.
- `guidelines` comes along when listing the vault's root without a cursor, so an agent that starts browsing
  a vault sees its rules.
- A document whose file can't be parsed is listed with `status: broken` and its path as title.
- Titles are cut to 200 bytes and descriptions to 500.

The text rendering has one line per folder and one per document (path, title, version), says when the vault
is empty or more entries follow, and puts the guidelines in a `guidelines` fence.

## Errors

| Code | When |
|---|---|
| `invalid` | Both `vault` and `repository`; a malformed `repository`; a cursor from another listing |
| `not_found` | No such vault the user can read; a folder other than the root with nothing under it |
| `unavailable` | The vault is building, failed or gone |
