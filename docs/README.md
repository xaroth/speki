# Speki documentation

Start with [what speki is](functional/overview.md).

Functional documentation describes what speki does, for the people running it and the people (and agents)
using it:

- [overview.md](functional/overview.md): what speki is, the words we use, the tools
- [vaults.md](functional/vaults.md): turning a repository into a vault, `speki.yaml`
- [documents.md](functional/documents.md): document format, frontmatter, paths, links
- [access.md](functional/access.md): signing in, who gets in, what they can do
- [tools.md](functional/tools.md): every MCP tool, its input and output
- [search-and-query.md](functional/search-and-query.md): how search ranks, what `query` can see
- [running.md](functional/running.md): GitHub App setup, configuration, deployment

Design documentation describes how it's built:

- [architecture.md](design/architecture.md): packages, the request path, startup
- [oauth.md](design/oauth.md): sign-in, tokens, the authorization server
- [sync.md](design/sync.md): vault discovery, mirrors, polling, writes
- [index.md](design/index.md): the SQLite index, search, the query sandbox
- [document.md](design/document.md): parsing and changing documents
- [mcp.md](design/mcp.md): the MCP server and its instructions
- [operations.md](design/operations.md): configuration, telemetry, builds

Research notes record what we looked at and why we chose what we did:
[MCP](research/mcp-protocol.md), [authentication](research/authentication.md),
[search engine](research/search-engine.md), [git access](research/git-access.md),
[storage format](research/storage-format.md), [security](research/security.md).

[PLAN.md](PLAN.md) is the build plan. [CONVENTIONS.md](CONVENTIONS.md) has the code rules.
