# guide

Explains speki and lists the vaults the user can use, with their descriptions and writing guidelines.
Agents call it once, before they first use speki in a conversation.

The server instructions are short because clients cut them at 2,048 characters. `guide` carries the
longer version: when to search and when to write, the document format and core fields, link style, how
versions and conflicts work, how search treats words and the vaults covering a repository, how long documents are read in parts and
history in pages, when to use `query`, what the access and availability errors mean, and
the trust rule for document text. It also carries what instructions can't: the vaults this user can see,
which differ per user.

## Input

None.

## Output

```json
{ "guide": "# Using speki\n\nSpeki holds the team's written knowledge: ...",
  "vaults": [
    { "vault": "acme/kb", "title": "Infrastructure knowledge",
      "description": "How our services are deployed and operated...",
      "frontmatter": ["owner", "review_by"],
      "repositories": ["github.com/acme/infra", "github.com/acme/svc-*"],
      "can_write": true, "documents": 412 },
    { "vault": "acme/new", "title": "New team notes", "description": "...",
      "can_write": false, "documents": 0, "state": "building" }
  ] }
```

| Field | Meaning |
|---|---|
| `guide` | The fixed explanation, embedded in the binary. With `instance.title` or `instance.description` set, it opens with them |
| `vault`, `title`, `description` | From the vault's `speki.yaml` |
| `guidelines` | Not in the structured result: the vault's writing rules for agents are in the text only, in a `guidelines` fence |
| `frontmatter` | Extra frontmatter keys agents may set in this vault, from `frontmatter.allow` |
| `repositories` | The code repositories the vault covers, as `host/owner/repo` |
| `can_write` | Whether this user, in this session, can change documents in the vault |
| `documents` | How many documents the vault has |
| `state` | `building`, `failed` or `gone` for a vault speki can't serve right now. Left out when served |

Vaults are sorted by name. An unserved vault has no `frontmatter`, and the text leaves out its guidelines:
its configuration may be out of date, and it can't be written to yet anyway.

The text rendering is the guide followed by a "Your vaults" section: one heading per vault, then its title,
description, repositories, and either "Access: read only" or "Access: read and write" with the document
count, or "Not available:" with the reason. A served vault with extra frontmatter keys adds "Frontmatter
keys you may also set: ...". Each served vault's guidelines sit in a `guidelines` fence, because anyone who
can push to the vault can write them ([overview.md](overview.md#results)). When no vault is served yet, it
says "None is ready yet. Try again in a minute."

## Errors

None of its own. The checks before any tool runs still apply ([overview.md](overview.md#before-a-tool-runs)).
