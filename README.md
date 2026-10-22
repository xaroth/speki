# speki

Speki is a knowledge base for AI agents. Teams keep what they know about their systems and decisions as
markdown in GitHub repositories, and agents such as Claude Code or the Claude app search, read and update
it over MCP (Model Context Protocol). GitHub decides who can see and change what.

- Knowledge lives in git. Every change an agent makes is a commit authored by the user it acts for.
- Users sign in with their GitHub account. They see the vaults (repositories) they can read on GitHub,
  and write where they can push.
- Search is full-text with stemming and typo tolerance. Read-only SQL covers what search can't, such as
  "runbooks not updated this year" or "what links here".
- Speki keeps no data of its own. Its search index is rebuilt from git, so an instance can be thrown away.

## Getting started

Speki is a single Go binary. Building it needs Go and a C compiler (the search index uses SQLite):

```sh
task build
```

Without [Task](https://taskfile.dev):

```sh
CGO_ENABLED=1 go build -tags 'sqlite_fts5,sqlite_omit_load_extension' -o bin/speki ./cmd/speki
```

Or pull the container image, published for each release:

```sh
docker pull ghcr.io/xaroth/speki:latest
```

Or build it:

```sh
docker build -t speki .
```

To run it you need a GitHub App, a configuration file and a secret key.
[docs/running.md](docs/running.md) walks through all three.
[config.example.yaml](config.example.yaml) is a starting point for the configuration.

Once speki is running at its public URL, turn a repository into a vault
([vaults.md](docs/vaults.md)) and connect a client:

```sh
claude mcp add --transport http speki https://speki.example.com/mcp
```

## Documentation

[docs/README.md](docs/README.md) lists everything. Good places to start:

- [What speki is](docs/overview.md)
- [Running speki](docs/running.md)
- [The tools agents use](docs/tools/overview.md)
