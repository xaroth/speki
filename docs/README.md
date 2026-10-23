# Speki documentation

Start with [what speki is](overview.md): its design principles explain most of what the other pages
describe.

How speki works:

- [overview.md](overview.md): what speki is, its design principles, how the parts fit together
- [vaults.md](vaults.md): turning a repository into a vault, `speki.yaml`, who may read and write, how speki
  keeps its copy current
- [documents.md](documents.md): the document format, frontmatter, links, paths, and what a change may do
- [changes.md](changes.md): how a change becomes a commit: versions, conflicts, lost answers, catching up
- [search.md](search.md): how search matches, corrects and ranks, and why it's SQLite FTS5
- [query.md](query.md): the views agents can query with SQL, and the sandbox that keeps them in their vaults
- [access.md](access.md): signing in, who gets in, permissions, tokens and refreshing
- [security.md](security.md): the threats speki is designed against, and what answers each

The MCP tools agents use:

- [tools/overview.md](tools/overview.md): what every tool shares: instructions, checks before a tool runs,
  results, fencing, errors
- [guide](tools/guide.md), [list](tools/list.md), [search](tools/search.md), [read](tools/read.md),
  [history](tools/history.md), [schema](tools/schema.md), [query](tools/query.md)
- [write](tools/write.md), [edit](tools/edit.md), [move](tools/move.md), [delete](tools/delete.md)

Running it:

- [running.md](running.md): GitHub App, secrets, configuration, Docker Compose, connecting clients, keys,
  building
- [operating.md](operating.md): ports, startup and shutdown, disk and memory, several instances, status,
  logs, metrics, traces

[CONVENTIONS.md](CONVENTIONS.md) has the code rules.
