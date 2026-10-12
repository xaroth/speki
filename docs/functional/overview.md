# What speki is

Speki is a knowledge base for agents. It stores what a team knows about its systems and decisions as
markdown in GitHub repositories. AI agents reach it over MCP (Model Context Protocol, the way Claude and
other assistants connect to tools). An agent searches speki before answering, and writes back what it
learned, so the next one starts from there.

Speki owns nothing. The knowledge lives in git, identity and permissions come from GitHub, and the
search index is rebuilt from git whenever speki starts. Delete a speki instance and nothing is lost.

## Words we use

Vault
: A GitHub repository that holds knowledge. A repository becomes a vault when the speki GitHub App is
  installed on it and it contains a `speki.yaml` file. Vaults are named by their repository:
  `eve-online-tools/kb`. See [vaults.md](vaults.md).

Document
: One markdown file in a vault. It starts with frontmatter: a block of YAML between `---` lines holding
  the title, tags and other properties. Its path within the vault is its identity. See
  [documents.md](documents.md).

Version
: A fingerprint of a document's content (its git blob SHA). Tools that change a document take the
  version the agent read, so nobody overwrites a change they haven't seen.

User
: A GitHub account. Speki knows users only through GitHub. There are no speki accounts.

Agent
: The MCP client acting for a user, like Claude Code or the Claude app. It has the user's access, or
  less if the user signed in read-only.

## What a user can do

A user who signs in sees the vaults they can read on GitHub, as long as speki's app is installed there.
In those vaults they can search, list, read and run queries. In vaults where they can push on GitHub they
can also write, edit, move and delete documents, unless they signed in read-only. A vault's `speki.yaml` can narrow
both to listed users and teams.

Every change is a commit on the vault's default branch, authored by the user. The git history is the audit
log.

An operator can require users to be members of one or more GitHub organisations. Anyone else is turned
away at sign-in and on every request, even if they can read a vault.

## The tools

| Tool | What it does |
|---|---|
| `guide` | Explains speki, lists the user's vaults with their descriptions and writing guidelines |
| `list` | Lists vaults, or the folders and documents under a path in a vault |
| `search` | Ranked full-text search across the user's vaults |
| `schema` | Describes the views available to `query` |
| `query` | Runs a read-only SQL query over the user's vaults |
| `read` | Reads a document, or a range of its lines |
| `write` | Creates a document or replaces it entirely |
| `edit` | Replaces an exact piece of text in a document |
| `move` | Moves or renames a document |
| `delete` | Deletes a document |
| `history` | Shows recent commits for a vault or a document |

[tools.md](tools.md) has the details.

## What speki doesn't do

- It stores markdown only. Images and other files in a vault are left alone and not indexed.
- It doesn't do semantic or embedding search. Search is full-text with stemming and typo tolerance.
- It doesn't open pull requests. Writes go straight to the default branch. A vault whose branch rules
  forbid that is read-only through speki.
- It has no admin interface. Configuration is a file plus environment variables, and vaults are managed
  in GitHub.
