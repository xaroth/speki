# What speki is

Speki is a knowledge base for AI agents. A team keeps what it knows about its systems and decisions as
markdown in GitHub repositories. Agents such as Claude Code or the Claude app reach it over MCP (Model
Context Protocol): they search speki before answering, and write back what they learned, so the next agent
starts from there.

## Design principles

These choices explain most of how speki behaves. The other pages refer back to them.

### Speki owns nothing

Knowledge lives in git. Identity and permissions come from GitHub. The search index is built from git and
can be thrown away. Speki has no database, no user table and no admin interface. Deleting an instance loses
nothing, and a new one on an empty disk rebuilds everything it needs.

This is why every change is a git commit on the vault's default branch, authored by the user, and why the
git history is the audit log. It's also why speki's tokens carry everything it needs to know about a user,
sealed so only speki can open them, instead of pointing at a session table ([access.md](access.md)).

### GitHub decides who sees what

A user sees a vault when they can read its repository on GitHub, and changes it when they can push to it.
Speki adds no permission model of its own. A vault's `speki.yaml` can only narrow GitHub's answer, never
widen it ([vaults.md](vaults.md#limiting-who-reads-and-writes-a-vault)). Writes go to GitHub with the
user's own token, so GitHub checks push access, branch rules and push protection at the moment of the
write, whatever speki thinks.

### Files stay normal

People clone vaults, open them in Obsidian and browse them on github.com. So a document is a plain markdown
file with YAML frontmatter, links are relative markdown links, and paths avoid characters that break
Obsidian, Windows or macOS. Speki only rewrites the parts of a file it has to, and keeps keys, comments and
line endings it doesn't own ([documents.md](documents.md)).

### Agents are the main readers

Tool descriptions, error messages and the `guide` text are written for a model. Every error starts with a
code and says what to do next. Results carry versions in full, because agents copy them into the next call.
Results that skip vaults say so, so an agent doesn't read silence as absence ([tools/](tools/overview.md)).

### Document text is data

Anyone who can push to a vault can put text in front of every agent that reads it. Speki can't make that
safe, but it marks the boundary: document content and vault guidelines come back between fences content
can't close, and the instructions tell agents to treat them as information, not instructions
([security.md](security.md#prompt-injection-stored-in-documents)).

### Small and boring

Speki is sized for a team: up to about 20 vaults of up to 50,000 documents each. That size allows simple
answers. One SQLite file per vault holds the whole index. Full-text search with stemming and typo
correction covers what agents need without embeddings. A shallow git copy per vault is cheap to keep and
cheap to rebuild.

## Words

Vault
: A GitHub repository that holds knowledge. A repository becomes a vault when the speki GitHub App is
  installed on it and it contains a `speki.yaml` file. Vaults are named by their repository, `acme/kb`.
  See [vaults.md](vaults.md).

Document
: One markdown file in a vault. It starts with frontmatter, a block of YAML between `---` lines holding the
  title, tags and other properties. Its path in the vault is its identity. See [documents.md](documents.md).

Version
: The git blob SHA of a document's content, 40 hex characters. Tools that change a document take the
  version the agent read, so nobody overwrites a change they haven't seen. See [changes.md](changes.md).

User
: A GitHub account. There are no speki accounts.

Agent
: The MCP client acting for a user, like Claude Code or the Claude app. It has the user's access, or less
  when the user signed in read-only.

Instance
: One running speki process. Several instances can share a deployment through Redis
  ([operating.md](operating.md#one-instance-or-several)).

## How the parts fit together

```text
            MCP clients (Claude Code, Claude apps, ...)
                         │ HTTPS (TLS terminated in front)
┌────────────────────────▼─────────────────────────────────────┐
│ speki                                                        │
│                                                              │
│  /oauth/*, /.well-known/*   sign-in, in front of GitHub      │
│                                                              │
│  /mcp   token check, rate limit, access grant, tools         │
│                          │                                   │
│                  per vault: shallow git copy + SQLite index  │
│                                                              │
│  shared state: memory, or Redis for several instances        │
└──────────────────────────────────────────────────────────────┘
        │ GitHub REST/GraphQL           │ git fetch
        ▼                               ▼
     api.github.com                 github.com (vault repositories)
```

Reading never asks GitHub for content. Speki keeps a shallow copy of every vault's default branch and a
search index built from it, polls GitHub for new commits every minute, and serves reads and searches from
those copies. Writes go the other way: a change becomes a commit through GitHub's API, made as the user,
and speki then catches its own copy up before answering ([changes.md](changes.md)).

Every request to `/mcp` passes the same checks in order: a cross-origin check, a 2 MiB body limit, the
token check, a per-user rate limit, and the access grant, which resolves what the user may read and write
from GitHub. Only then does a tool run. The rate limit runs before the grant, so a user who is turned away
can't make speki ask GitHub about them without limit.

Inside speki, one rule matters most: vault content is only handed out after checking the user's grant.
The code that reads vault copies is in one package, and every function in it that touches a vault takes
the grant. A vault the user can't read behaves exactly like one that doesn't exist.

## What a user can do

A user who signs in sees the vaults they can read on GitHub, where speki's app is installed. In those they
can list, search, read, query and see history. Where they can push, they can also write, edit, move and
delete documents, unless they signed in read-only. An operator can require membership in one or more
GitHub organizations.

The [tools](tools/overview.md) are:

| Tool | What it does |
|---|---|
| [`guide`](tools/guide.md) | Explains speki and lists the user's vaults with their descriptions and writing guidelines |
| [`list`](tools/list.md) | Lists vaults, or the folders and documents under a path in a vault |
| [`search`](tools/search.md) | Ranked full-text search across the user's vaults |
| [`schema`](tools/schema.md) | Describes the views `query` can use |
| [`query`](tools/query.md) | Runs one read-only SQL query over the user's vaults |
| [`read`](tools/read.md) | Reads a document, or a range of its lines |
| [`write`](tools/write.md) | Creates a document or replaces it entirely |
| [`edit`](tools/edit.md) | Replaces an exact piece of text in a document |
| [`move`](tools/move.md) | Moves or renames a document |
| [`delete`](tools/delete.md) | Deletes a document |
| [`history`](tools/history.md) | Shows recent commits for a vault or a document, a page at a time |

## What speki doesn't do

- It stores markdown only. Images and other files in a vault are left alone and not indexed.
- It has no semantic or embedding search. Search is full-text with stemming and typo tolerance.
- It doesn't open pull requests. Writes go straight to the default branch, and a vault whose branch rules
  forbid that is read-only through speki.
- It has no admin interface. Configuration is a file plus environment variables, and vaults are managed in
  GitHub.
- It doesn't rewrite links when a document moves. `move` and `delete` list the documents that link to the
  old path, so the agent can fix them.
