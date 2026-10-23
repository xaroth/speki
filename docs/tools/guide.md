# guide

Lists the vaults the user can use, with their guidelines, and explains how to search them well and how
documents are written. Agents call it once, early in a conversation.

The server instructions are short because clients cut them at 2,048 characters. `guide` carries the
longer version: when to search and when to write, how search treats words and the vaults covering a
repository, how to browse, query, read long documents in parts and page through history, how changes,
versions and conflicts work, the document format and core fields, link style, what an error looks like,
and the trust rule for fenced text. It also carries what instructions can't: the vaults this user can
see, which differ per user.

## Input

None.

## Output

The guide, embedded in the binary from `mcpserver/guide.md`, followed by a "Your vaults" section. With
`instance.title` or `instance.description` set, the guide opens with them, for agents connected to more
than one speki.

```text
# Using speki

Each vault is a GitHub repository of markdown documents. ...

## Your vaults

### acme/kb

Title: Infrastructure knowledge
Description: How our services are deployed and operated.
Repositories: github.com/acme/infra, github.com/acme/svc-*
Access: read and write. Documents: 412
Frontmatter keys you may also set: owner, review_by
<guidelines boundary="9f3c1a7e5b2d8c40">
One page per service. ...
</guidelines boundary="9f3c1a7e5b2d8c40">

### acme/new

Title: New team notes
Not available: still building its index; try again shortly
```

Each vault has a heading, sorted by name, then the lines that apply:

| Line | Meaning |
|---|---|
| `Title`, `Description` | From the vault's `speki.yaml` |
| `Repositories` | The code repositories the vault covers, as `host/owner/repo` |
| `Access` | "read only" or "read and write" for this user in this session, and how many documents the vault has |
| `Not available` | Instead of `Access`, for a vault speki can't serve right now: building, failed or gone, and what to do |
| `Frontmatter keys you may also set` | Extra keys agents may set in this vault, from `frontmatter.allow` |
| `guidelines` fence | The vault's writing rules for agents. Fenced, because anyone who can push to the vault can write them ([overview.md](overview.md#results)) |

An unserved vault has no frontmatter keys and no guidelines: its configuration may be out of date, and it
can't be written to yet anyway. When no vault is served yet, the section starts with "None is ready yet.
Try again in a minute."

## Errors

None of its own. The checks before any tool runs still apply ([overview.md](overview.md#before-a-tool-runs)).
