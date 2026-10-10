# Configuration, telemetry and builds

## Configuration

The configuration has two halves.

`service.Config` is the typed configuration speki runs with. It knows nothing about files or environment
variables:

```go
// Package service
type Config struct {
	PublicURL     string    `yaml:"public_url"`
	Listen        string    `yaml:"listen"`
	MetricsListen string    `yaml:"metrics_listen"`
	GitHub        GitHub    `yaml:"github"`
	Access        Access    `yaml:"access"`
	Auth          Auth      `yaml:"auth"`
	Sync          Sync      `yaml:"sync"`
	Redis         Redis     `yaml:"redis"`
	Telemetry     Telemetry `yaml:"telemetry"`
}
```

`redis.url` is a secret (`redis://` or `rediss://`, password included), so it can come from
`redis.url_file`. Without it, the store and change notices are in memory. With it, `service.New` connects
and fails at startup when Redis doesn't answer.

Zero values mean "use the default", and `service.New` fills them in before checking the result. It
collects every problem (a missing app ID, a `public_url` that isn't `https`, a duration of zero) into one
error, so an operator fixes them all in one go. Secrets have a `Secret` type whose `String` and `LogValue`
methods return `[redacted]`, so printing a config is safe.

`cmd/speki/config.go` produces that struct:

```go
// package main
func loadConfig(path string, environ []string) (service.Config, error)
```

1. Decode the YAML file named by `--config` (default `config.yaml`), with unknown keys rejected: a typo in
   a key should fail, not be silently ignored. A missing file is fine when everything comes from the
   environment.
2. Walk the struct by reflection and apply environment variables named after the `yaml` tags:
   `SPEKI_GITHUB_CLIENT_SECRET` for `github.client_secret`.
3. Read the `..._file` fields from disk into their secrets.

Durations use Go syntax (`60s`, `5m`). `environ` comes from `os.Environ()` in `main`, so tests pass their
own and don't touch the process environment.

The command has two flags: `--config <path>` and `--version`.

## Logging

`log/slog` with the JSON handler on stderr. `telemetry.NewLogger` wraps it with a `ReplaceAttr` that redacts
attributes named `token`, `authorization`, `code`, `state`, `cookie`, `refresh_token`, `content`, `sql`,
`old_text` and `new_text`, as a safety net under the rule not to log them. Speki's own attributes avoid
those names (the tool outcome is `outcome`, not `code`).

Every request gets a request ID, taken from an incoming `X-Request-ID` if it's a plain token of at most 64
characters, otherwise generated. It's in every log line, in error messages to clients, and on the trace
when tracing is on.

Every tool call logs one line at the end: request ID, GitHub login, client name, tool, vault, path,
`outcome` (the error code, `ok`, or `canceled` when the client went away) and duration. Background work logs per vault: discovery changes, sync results, index builds with
duration and document count, failures.

## Metrics

Prometheus, on the internal `metrics_listen` server. `service` starts it as its own `http.Server` with its
own mux, separate from the public server on `listen`, so a routing mistake on the public side can't expose
it. `service.New` rejects a configuration where `metrics_listen` equals `listen`, and the end-to-end tests
check that `/metrics` and `/status` return 404 on the public server.

Names:

| Metric | Type | Labels |
|---|---|---|
| `speki_tool_calls_total` | counter | `tool`, `outcome` (as in the log line) |
| `speki_tool_duration_seconds` | histogram | `tool` |
| `speki_github_requests_total` | counter | `kind` (app, installation, user), `status` |
| `speki_github_rate_remaining` | gauge | `kind` |
| `speki_vault_up` | gauge | `vault`. 1 when the vault is served and its last sync succeeded |
| `speki_vault_documents` | gauge | `vault` |
| `speki_vault_bytes` | gauge | `vault`. Markdown bytes indexed |
| `speki_vault_fresh_seconds` | gauge | `vault`. Time since speki last confirmed the index matches GitHub's head |
| `speki_sync_total` | counter | `result` (unchanged, applied, rebuilt, failed) |
| `speki_signins_total` | counter | `result` |

`speki_vault_fresh_seconds` is the one to alert on: above a few poll intervals, a vault is falling behind.

Vault labels are fine at 20 vaults. User logins are never labels.

## Status page

`GET /status` on the metrics port returns a JSON list of vaults with their name, state (serving,
building, failed), index commit, document count and size, a warning when it's larger than speki is built
for, last successful sync, and last error. It's the first
place to look when someone says "my vault isn't showing up". Like `/metrics`, it's for operators only and
shouldn't be exposed publicly.

## Tracing

OpenTelemetry, off unless `telemetry.tracing` is true. The exporter is OTLP over HTTP, configured by the
standard `OTEL_EXPORTER_OTLP_*` variables. Spans: the HTTP request (`telemetry.Trace`, a new trace per request that keeps
an incoming `traceparent` only as a link), each tool call, GitHub requests (an `otelhttp` transport on the
GitHub client, which doesn't send speki's trace context to GitHub), `index.Search`, `index.Query`,
`index.Apply`, and `library.Sync`. Span attributes follow the logging rules: no content, no SQL, no tokens. The server span leaves out query strings,
because the GitHub callback carries `code` and `state` there, and names spans from a fixed set of routes.

## Builds

The SQLite driver needs cgo, so builds need a C compiler.

- `Taskfile.yml` with `build`, `test`, `lint`, `vet`, `vuln`, `fuzz`, `run` and `ci` tasks. Build tags
  `sqlite_fts5,sqlite_omit_load_extension`.
- CI (`.github/workflows/ci.yml`, on pull requests and pushes to `main`, skipping docs-only changes):
  the latest Go 1.27 with cgo, `golangci-lint` (govet and staticcheck included) and `go test -race`, the
  same as a local `task ci`. `go test` runs the `FuzzParseTouch` seed corpus. `docker build` runs on
  pushes to `main` only.
- `govulncheck` (`vuln.yml`) runs on pull requests that change `go.mod` or `go.sum`, and monthly.
- Fuzzing (`fuzz.yml`) runs `FuzzParseTouch` on manual dispatch only, for 5 minutes by default.
- The image `EXPOSE`s only the public port (8080). The metrics port is reachable on the container network
  but not published by default.
- The image is built in two stages: `golang:1.27-bookworm` to build, `gcr.io/distroless/base-debian12` to
  run, as a non-root user, with the data directory as a volume.

`go.mod` pins versions. Dependencies, and why:

| Module | For |
|---|---|
| `github.com/modelcontextprotocol/go-sdk` | MCP server and resource-server auth |
| `github.com/mattn/go-sqlite3` | Index |
| `github.com/go-git/go-git/v5` | Mirrors |
| `github.com/google/go-github` (latest major, pinned in `go.mod`) | GitHub REST |
| `github.com/bradleyfalzon/ghinstallation/v2` | App JWT and installation tokens |
| `github.com/yuin/goldmark` | Markdown |
| `go.yaml.in/yaml/v3` | YAML |
| `golang.org/x/text` | Unicode normalisation |
| `golang.org/x/sync` | `errgroup` |
| `golang.org/x/time` | Rate limiter |
| `github.com/redis/go-redis/v9` | Redis and Valkey |
| `github.com/prometheus/client_golang` | Metrics |
| `go.opentelemetry.io/otel` and the `otelhttp` contrib package | Tracing |
| `github.com/google/go-cmp` | Tests |

GraphQL is one mutation, sent as a plain POST from `gh`, so there's no GraphQL client dependency.
