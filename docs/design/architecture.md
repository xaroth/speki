# Architecture

How speki is put together: the packages, what each one owns, and how a request moves through them. The
other design documents go into detail per area:

- [oauth.md](oauth.md): sign-in, tokens and the authorization server
- [sync.md](sync.md): discovering vaults, the local copies, polling and writing
- [index.md](index.md): the search index, search and the query sandbox
- [document.md](document.md): parsing, validating and changing documents
- [mcp.md](mcp.md): the MCP server, tools and instructions
- [operations.md](operations.md): configuration, logs, metrics, traces and builds

## Shape

```text
            MCP clients (Claude Code, Claude apps, ...)
                         │ HTTPS (TLS terminated in front)
┌────────────────────────▼─────────────────────────────────────┐
│ speki                                                        │
│                                                              │
│  /oauth/*, /.well-known/*  ──► oauth ──► gh                  │
│                                                              │
│  /mcp ──► mcpserver: origin check, oauth token check,        │
│           access grant, rate limit, MCP handler              │
│                          │                                   │
│                      knowledge ──► gh (commits, history)     │
│                          │                                   │
│                       library ──► gitsync (git objects)      │
│                          │    └─► index (SQLite per vault)   │
│                          ▼                                   │
│                        vault (registry, speki.yaml)          │
│                                                              │
│  store: memory, or Redis when clustered                      │
└──────────────────────────────────────────────────────────────┘
        │ GitHub REST/GraphQL           │ git fetch
        ▼                               ▼
     api.github.com                 github.com (vault repos)
```

Speki keeps two kinds of state, and both can be thrown away:

- Per instance, on disk: a shallow git copy and a SQLite index per vault, rebuilt from GitHub when missing.
- Short-lived coordination: used authorization codes, refresh results, and change notices between
  instances. In memory for one instance, in Redis for several.

## Packages

One module, `github.com/eve-online-tools/speki`. The command lives in `cmd/speki`, and every other package
sits at the top level, named after what it does. No `internal/`, no `util`, no layers named `repository`.

| Package | Owns | Imports from speki |
|---|---|---|
| `cmd/speki` | The process: flags, reading the config file and environment, the logger, signals, exit codes | `service`, `telemetry` |
| `service` | Speki as one runnable thing: `service.Config`, validation and defaults, building every component, the HTTP servers, startup and shutdown order | everything below |
| `telemetry` | slog handler with redaction and request IDs, OpenTelemetry tracer and server spans. Each package registers its own metrics; `service` owns the Prometheus registry | |
| `seal` | Keyring, sealing and opening typed blobs with AES-256-GCM | |
| `store` | `Memory` and `Redis`: single-use codes, refresh locks and results, and change notices between instances | |
| `gh` | GitHub API: app JWT, installation tokens, user OAuth, user and installation lookups, commits, history | |
| `document` | Parsing and writing documents: frontmatter, core fields, validation, links, headings, path rules | |
| `vault` | The `Vault` type, `speki.yaml` parsing, the registry of current vaults and its discovery loop | `gh` |
| `gitsync` | A shallow git copy of one repository: clone, fetch, tree diff, blobs | |
| `index` | One SQLite index file: schema, build, apply changes, search, listing, and the query sandbox over several index files | `document` |
| `library` | The local copy of every vault (a `gitsync` mirror plus an `index`), keeping them in step with GitHub: the poll loop, sync and write locks, change notices | `vault`, `gitsync`, `index`, `document` |
| `access` | Turning a signed-in user into a `Grant`: org gate, readable and writable vaults, caching | `gh`, `vault` |
| `oauth` | Authorization server endpoints, consent page, speki tokens, bearer token check | `gh`, `seal` |
| `knowledge` | What the tools do: list, search, read, write, edit, move, delete, history, query. Enforces the `Grant` | `access`, `document`, `library`, `vault` |
| `mcpserver` | The `/mcp` HTTP stack, tool definitions, instructions text, mapping results and errors | `knowledge`, `access`, `oauth` |

`gitsync` and `index` know nothing about vaults or users. Each works on one repository or one database
file, which makes them easy to test on their own. `library` is where a vault's git copy and index meet,
and the only place that decides when to sync. `knowledge` knows nothing about MCP, so its behaviour is
tested without a protocol in the way.

Where a package needs something from another package, it declares a small interface for exactly what it
uses, next to the code that uses it. `knowledge` declares a `committer` interface with the one `gh` method
it calls, and tests hand it a fake. Concrete types everywhere else. No interface per package "just in case".

## The `Grant` type

The rule that matters most for security: content is only handed out after checking a `Grant`.

```go
// Package access

// Grant is what one signed-in user may do right now, resolved from GitHub and the token's scope.
type Grant struct {
	user     User
	readable []vault.Vault // sorted by name
	writable map[int64]bool
}

func NewGrant(u User, readable []vault.Vault, writable []int64) *Grant // used by Resolver and tests

func (g *Grant) User() User                          // GitHub ID, login, GitHub token, client name
func (g *Grant) Vault(name string) (vault.Vault, bool) // a readable vault, by name
func (g *Grant) Readable() []vault.Vault
func (g *Grant) CanWrite(repoID int64) bool
func (g *Grant) ReadOnlySession() bool
```

Agents name vaults, so `knowledge` looks a vault up through `grant.Vault(name)`: a vault the user can't
read doesn't resolve, and the answer is the same `not_found` as for a vault that doesn't exist.

Vaults are keyed by repository ID internally, so a renamed or transferred repository keeps its grant,
its cache entries and its files. People and agents use the `owner/repo` name.

`knowledge` is the only package that calls `library`'s read methods, and every one of its functions takes
a `*access.Grant` and checks it before touching a vault. `library` and `index` take repository IDs and trust
their caller. Two review rules keep this honest:

- Only `knowledge` and `service` import `library`.
- Every exported `knowledge` function that touches a vault takes a `*access.Grant`.

The user's GitHub token is reachable only through `Grant.User()`, so the one place where speki acts as the
user with GitHub (commits, history) is easy to find.

A read-only sign-in yields a `Grant` with no writable vaults, regardless of GitHub permissions. Write tools
then fail with `read_only_session`.

## The `/mcp` stack

`mcpserver.Handler` builds the whole chain, so it can be read in one place:

1. `http.NewCrossOriginProtection`: rejects cross-origin browser requests, as the MCP spec asks.
2. `http.MaxBytesHandler`, 2 MiB.
3. `oauth.Middleware`: opens the bearer token, checks audience and expiry, puts the `oauth.Identity` on the
   context. A bad token is a 401 with the `WWW-Authenticate` header that starts sign-in.
4. Per-user rate limiter: 10 requests a second, bursts of 30, per instance. Over the limit is an HTTP 429
   with `Retry-After`. It runs before the grant, so a user who is turned away can't make speki ask GitHub
   about them without limit.
5. Grant middleware: calls `access.Resolver.Resolve` and puts the `Grant` on the context. When GitHub
   rejects the user's GitHub token, it answers 401 (the client refreshes, which fails, and the user signs in
   again). When the org gate or the "at least one vault" rule fails, it answers 403 with a message saying
   which.
6. The go-sdk handler, which routes to the tool handlers.

Doing steps 3 and 4 as HTTP middleware matters: inside a tool handler it's too late to answer with a
status code, and clients only start a new sign-in on a 401.

## A tool call, start to end

`search` for "esi cache":

1. The `/mcp` stack above runs. `Resolve` finds the user's GitHub permissions in its 60-second cache, keyed
   by a hash of the GitHub token, and applies the token's scope.
2. The `search` tool handler converts its input and calls `knowledge.Search(ctx, grant, request)`.
3. `knowledge` picks the vaults: the requested ones, each looked up with `grant.Vault`, or all readable
   ones, and takes each one's index from `library`.
4. `index.Search` runs the search on every index in parallel and merges the results. It searches all of
   them at once because typo correction needs every vault's vocabulary.
5. The handler turns the result into structured content plus a text rendering.

`edit` takes these steps after the grant:

1. `knowledge.Edit` checks `grant.CanWrite` and takes the vault's write lock.
2. It reads the current document. If the agent passed a `version` that doesn't match, it first asks
   `library` to check GitHub for a newer head (a conditional request, free when nothing changed) and sync.
   Only if the document still doesn't match does it return `conflict`.
3. It replaces `old_text`, parses the result with `document`, checks that the change didn't introduce new
   problems, and sets `updated`.
4. It commits through `gh` with the user's token, conditional on the head commit `library` last saw. If
   GitHub says the head moved, `knowledge` syncs and goes back to step 2, up to three times.
5. On success it computes the new version (the git blob hash of the content it sent), asks `library` to
   sync, and waits until a sync that started after the commit has finished (at most 10 seconds). Then it
   publishes the new head for other instances and returns.

Lock order is always the vault's write lock first, then its sync lock. Nothing takes them the other way
around.

## The command and the service

`cmd/speki` is the only code that touches the process: `os.Args`, `os.Environ`, the config file, signals
and the exit code. It turns them into a `service.Config` and a logger, and hands those over.

```go
// cmd/speki/main.go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Environ(), os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "speki:", err)
		os.Exit(1)
	}
}

// run parses flags, loads the configuration (config.go), builds the logger and runs the service.
func run(ctx context.Context, args, environ []string, stderr io.Writer) error
```

`cmd/speki/config.go` holds the loading: the `--config` flag, decoding the YAML, applying `SPEKI_*`
environment variables, and reading `..._file` secrets. See [operations.md](operations.md#configuration).
`run` takes its inputs as arguments, so it's tested without touching the real environment.

`service` is speki without the process around it:

```go
// Package service

// New checks cfg, fills in defaults and builds every component. Nothing runs yet.
func New(cfg Config, logger *slog.Logger) (*Service, error)

// Run starts the HTTP servers and background loops, and blocks until ctx ends and shutdown is done.
func (s *Service) Run(ctx context.Context) error
```

Because `New` takes a plain struct and a logger, tests build a complete speki in-process: a `Config`
pointing at a fake GitHub and a temporary data directory, then `Run` with a context they cancel. Swapping a
dependency for a fake happens through `Config`, not through globals or build tags.

## Startup and shutdown

`service.New` does steps 1 and 2, `Run` the rest:

1. Check the configuration and fill in defaults. `New` returns an error naming every bad key at once, and
   `cmd/speki` prints it and exits.
2. Set up the metrics registry and tracer, the keyring, the store (memory or Redis), the GitHub client, and
   every other component. Nothing connects to anything yet.
3. Start the HTTP servers. `/healthz` answers right away. `/readyz` and `/mcp` answer 503 until step 5.
4. Remove leftovers of interrupted index builds. Open every vault that already has a usable copy and
   index on disk, so it can be served as is.
5. Run discovery once. Mark ready. A vault that isn't on disk yet is cloned and indexed in the background
   (4 at a time). It shows up in tools once it's done. A clone or build that makes no progress for
   5 minutes is cancelled and retried later, so a hung connection can't hold a slot forever. Large vaults
   that are still making progress are left to finish.
6. Start the discovery loop, the poll loop, and the change subscription.

So an instance with a persistent data directory serves from disk within seconds, even if GitHub is down.
An instance on an empty disk is ready quickly but has no vaults until the first ones finish indexing.

When `Run`'s context ends, within 25 seconds in all:

1. Mark not ready, for good: `/readyz` and `/mcp` answer 503 from now on.
2. Stop the background loops and wait for them, so requests still running see open vaults.
3. Stop accepting requests and let running ones finish. When the 25 seconds are up, close their
   connections. That's logged, not an error: a routine stop exits 0.
4. Close indexes, the sandbox and the store, and return from `Run`.

`/mcp` also answers 503 with `Retry-After` before the first discovery round, like `/readyz`. Every
goroutine is started from `service` or `library` with a context it watches, and its starter waits for
it to end. `cmd/speki` turns `SIGTERM` into a cancelled context, and a second signal during the drain
ends the process at once. `service` never looks at signals itself.

## Errors

Packages wrap errors with `%w` and context about the operation. `knowledge` returns a `*knowledge.Error`
with a `Code` (`not_found`, `conflict`, `invalid`, ...), a message written for the agent, and sometimes
the current version. `mcpserver` turns that into a tool error. Any other error becomes `unavailable` with
a generic message and the request ID, and the full error is logged with the same request ID. Raw GitHub
and SQLite messages never reach the client, except SQL syntax errors from `query`, which are scrubbed first.

Every request gets a request ID (`telemetry`), whether tracing is on or not. Logs, error messages and
traces all carry it.

## Testing

- `document`, `seal` and `index` are plain code with table-driven tests and fixtures under `testdata/`.
- `cmd/speki` tests `run` and the config loading with given arguments, environment and files. The examples in the functional docs (stemming, typo correction, path rules) are test cases.
- `gitsync` tests run against local bare repositories made in a temporary directory.
- `access`, `oauth`, `knowledge`, `library` and `vault` get hand-written fakes of the small interfaces they
  declare.
- `gh` is tested against an `httptest` server that replays recorded GitHub responses.
- The end-to-end tests live in `service`. They start a `Service` with a fake GitHub (an `httptest` server
  for the API plus local bare repositories for git) and drive it with the go-sdk MCP client: sign in,
  search, edit, read back, shut down cleanly.
- An opt-in test (`-tags=github`) runs against a real test organisation. It's not part of CI.
