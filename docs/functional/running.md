# Running speki

Speki is one binary. It needs a GitHub App, a configuration file, a secret key and some disk it may lose at
any time. For more than one instance it also needs Redis or Valkey.

To get a first instance running:

1. Register the GitHub App and install it on a repository.
2. Write a configuration file and generate a key.
3. Build and start speki.
4. Add a `speki.yaml` to the repository ([vaults.md](vaults.md)) and connect an MCP client.

## Registering the GitHub App

Create a GitHub App in the organisation that runs speki (Settings → Developer settings → GitHub Apps):

| Setting | Value |
|---|---|
| Callback URL | `https://<speki host>/oauth/github/callback` |
| Expire user authorization tokens | on. Speki refuses tokens that don't expire |
| Request user authorization during installation | off |
| Webhook | off |
| Repository permissions | Contents: read and write. Metadata: read |
| Organization permissions | Members: read |
| Where can this app be installed | Only on this account (recommended) |

Don't grant any other permission. Without "Workflows", GitHub itself refuses writes to
`.github/workflows/`, which is what we want.

Then generate a private key and a client secret, and install the app on the vault repositories. If you
require organisation membership (below), install the app on each of those organisations too, with zero
repositories selected if it holds no vaults. GitHub only answers membership questions for organisations
where the app is installed.

## Configuration

Speki reads a YAML file (`--config`, else `SPEKI_CONFIG`, else `config.yaml` in the working directory) and then environment
variables, which win.

```yaml
public_url: https://speki.example.com

listen: ":8080"
metrics_listen: ":9090"

github:
  app_id: 123456
  client_id: Iv23liAbCdEf
  client_secret_file: /run/secrets/github-client-secret
  private_key_file: /run/secrets/github-app.pem
  # Numeric IDs of the accounts whose installations count (IDs, because names can be
  # given up and claimed by someone else). Defaults to the app's owner.
  allowed_accounts: [87654321]   # acme

access:
  # Users must be an active member of at least one. Empty means no org check.
  required_orgs: [acme]

auth:
  keys_file: /run/secrets/speki-keys.yaml
  # Hosts whose client metadata documents speki accepts (web clients like claude.ai).
  client_metadata_hosts: [claude.ai]

sync:
  data_dir: /var/lib/speki
  poll_interval: 60s
  discovery_interval: 5m

# Set to run more than one instance.
redis:
  url_file: /run/secrets/redis-url

telemetry:
  log_level: info
  tracing: false   # uses the standard OTEL_EXPORTER_OTLP_* variables when on

instance:          # optional: what this speki is, for people who use more than one
  name: work       # lower-case letters, digits and dashes, at most 32; the server is called speki-work
  title: Fenris work knowledge
  description: >   # what belongs here, at most 300 characters
    Documentation for Fenris projects and services: architecture, runbooks and decisions.
```

Without `instance`, speki calls itself `speki` and describes itself as the team's knowledge base. With
it, the title shows on the sign-in page and at the top of `guide`, and the description opens the
instructions agents get, so an agent connected to two spekis knows which to search and where to write.

Every key has an environment variable: `SPEKI_` plus the path in upper case with `_` between parts.
`github.client_secret_file` becomes `SPEKI_GITHUB_CLIENT_SECRET_FILE`. Secrets can be given directly
(`SPEKI_GITHUB_CLIENT_SECRET`) or as a file path (`..._FILE`). Files are better: they don't show up in
process listings or crash dumps. Any other text setting can come from a file the same way. Environment
variables win over the YAML file, including over a secret file it names, which then isn't read.

Speki refuses to start when something required is missing or a value is invalid, and lists every such
key at once.

Common settings you can leave out:

| Key | Default |
|---|---|
| `listen` | `:8080` |
| `metrics_listen` | `:9090` |
| `github.allowed_accounts` | the account that owns the GitHub App |
| `access.required_orgs` | no organisation check |
| `auth.client_metadata_hosts` | `claude.ai` |
| `sync.data_dir` | `data`, relative to the working directory |
| `sync.poll_interval` | `1m` |
| `sync.discovery_interval` | `5m` |
| `telemetry.log_level` | `info` (also `debug`, `warn`, `error`) |
| `sqlite.memory_limit_mib` | `1024`: SQLite's memory limit for the whole process. Searches, index builds and queries fail past it. 128 to 1048576 |
| `sqlite.query_memory_mib` | `384`: where agents' SQL queries are stopped with `too_large`. At most 3/4 of `sqlite.memory_limit_mib` |

Size `sqlite.memory_limit_mib` to fit well under the container's memory limit, next to git and Go's own
memory. Raise `sqlite.query_memory_mib` if agents' queries often fail with `too_large`, and lower it if
searches or syncs fail with out-of-memory errors while queries run.

`public_url` must be https, except for `http://localhost` or `http://127.0.0.1` during development, and
has no path.

### Keys

Speki seals its tokens with a key. Set exactly one of:

- `auth.key`: a single key, base64 of at least 32 random bytes. Easiest to set from a secret store:
  `SPEKI_AUTH_KEY_FILE=/run/secrets/speki-key`. It can't be rotated without first moving to a key file.
- `auth.keys`: the contents of a key file, which can hold several keys and supports rotation. Usually
  given as a path with `auth.keys_file`.

`openssl rand -base64 32` makes a good secret.

The key file looks like this:

```yaml
keys:
  - id: "2026-10"
    secret: "base64 of 32 random bytes"
    primary: true
  - id: "2026-04"
    secret: "..."
```

New tokens use the primary key. Any listed key can open existing tokens. To rotate:

1. Add a new key, not primary, on every instance. Restart.
2. Make it primary. Restart.
3. Remove the old key after 90 days, when the last refresh token sealed with it has expired.

Removing a key early signs everyone out who still holds tokens sealed with it. If a key may have leaked,
remove it and also revoke all user tokens in the GitHub App's settings: someone with the key could open
captured tokens and use the GitHub token inside directly.

To rotate away from `auth.key`, first move it into a key file under the ID `default`, which is the ID
speki gives the single key, so existing tokens keep opening:

```yaml
keys:
  - id: default
    secret: "the auth.key value"
    primary: true
```

Replace `auth.key` with `auth.keys` on every instance and restart. Then rotate as above, starting by
adding the replacement key. Listing the old key under any other ID signs everyone out.

## Building and starting

Build the binary with Go and a C compiler. The build tags enable SQLite's full-text search:

```sh
task build    # writes bin/speki
```

Without [Task](https://taskfile.dev):

```sh
CGO_ENABLED=1 go build -tags 'sqlite_fts5,sqlite_omit_load_extension' -o bin/speki ./cmd/speki
```

Start it with `bin/speki --config /path/to/config.yaml`, or set `SPEKI_CONFIG=/path/to/config.yaml`. With
neither it reads `config.yaml` in the working directory if there is one, and otherwise runs on environment variables alone. `speki --version`
prints the version.

Each release publishes an image to `ghcr.io/xaroth/speki` for `linux/amd64` and `linux/arm64`, tagged with
its version (`1.2.3`) and, for non-prereleases, `1.2` and `latest`. The image is signed with cosign, and
carries an SBOM and build provenance. To check that an image came from this repository's release workflow:

```sh
cosign verify ghcr.io/xaroth/speki:1.2.3 \
  --certificate-identity-regexp '^https://github.com/xaroth/speki/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect ghcr.io/xaroth/speki:1.2.3 --format '{{json .Provenance}}'
docker buildx imagetools inspect ghcr.io/xaroth/speki:1.2.3 --format '{{json .SBOM}}'
```

To build it yourself, the Dockerfile builds a small image that runs as a
non-root user:

```sh
docker build --build-arg VERSION=1.0.0 -t speki .
docker run -p 8080:8080 \
  -v /srv/speki/config.yaml:/etc/speki/config.yaml:ro \
  -v /srv/speki/secrets:/run/secrets:ro \
  -v speki-data:/var/lib/speki \
  speki
```

The image's working directory is `/etc/speki`, so it reads `/etc/speki/config.yaml`. Its data volume is
`/var/lib/speki`: set `sync.data_dir: /var/lib/speki` to use it. The image runs as user ID 65532, which
must be able to write there.

Once it runs, connect a client to `<public_url>/mcp`. For Claude Code:

```sh
claude mcp add --transport http speki https://speki.example.com/mcp
```

The first connection opens a browser to sign in ([access.md](access.md)).

## One instance or several

With no `redis` configured, speki runs as a single instance and keeps its short-lived state in memory:
used authorization codes, refresh locks, refresh results and the access cache (what GitHub said each
user may see, kept for 5 minutes). A restart loses that state, which only means a few users get checked
against GitHub again.

Run exactly one instance without Redis. Several instances with memory stores don't share refresh locks
and results: when a client's refreshes land on two of them, both use the same single-use GitHub refresh
token, and the user is signed out. Speki logs a warning at startup when it uses the memory store, and the
status page shows `"store": "memory"` with the same warning.

With `redis` configured, any number of instances can run behind a load balancer without sticky sessions.
They share that state through Redis, and when one instance writes to a vault, it tells the others, so
they pick up the change within seconds instead of at the next poll. They also share what discovery and
polling learn from GitHub, so N instances use about as much of the GitHub rate limit for that as one.
Every instance needs the same
configuration: the same `public_url`, GitHub App and keys, and the same Redis. Speki needs Redis 6.2 or
later, or Valkey, because it uses `GETDEL`. An instance that can't reach Redis at startup refuses to
start. If Redis goes away later, sign-in and refresh fail until it's back, every request checks access
with GitHub, and changes reach the other instances at their next poll.

The per-user rate limit (10 requests a second, bursts of 30) is kept by each instance, not shared through
Redis. With N instances behind a load balancer, a user can make up to N times as many requests.

Speki's keys in Redis are small and short-lived (an hour at most), all prefixed `speki:`. Configure Redis not to
evict them (`maxmemory-policy noeviction`): every speki key has a time to live, so policies such as
`volatile-ttl` would evict exactly the used-code markers, and an evicted one could let an authorization
code be used twice. Persistence isn't needed.

Every instance clones and indexes every vault itself. Plan disk and memory for that per instance
(see below).

## Disk and memory

Speki keeps a shallow clone and a search index per vault under `sync.data_dir`. Rough sizes per 10,000
documents of about 5 KB each: 50 MB of git objects and 150 MB of index. Use local disk. Network file
systems make SQLite slow and sometimes break its locking.

The data directory can be wiped at any time, but keeping it (a persistent volume) is better. With the
data on disk, a restarted instance serves its vaults within seconds, even when GitHub is unreachable, and
only catches up on what changed. On an empty disk, speki becomes ready quickly but each vault appears only
after it's cloned and indexed, about a minute for a vault of 50,000 documents, four vaults at a time.

A vault that fails to index (bad `speki.yaml`, GitHub unreachable) is logged and shown on the status page.
A bad `speki.yaml` is read again when the repository is next pushed to. A clone or sync that fails is
retried with backoff, from two minutes up to every 30 minutes, and the vault keeps serving its last good
index meanwhile. The other vaults aren't affected. A vault larger than speki is
built for (over 50,000 documents or 500 MB of markdown) is indexed anyway, with a warning in the logs and
on the status page. Plan disk, memory and startup time for it.

To force a vault to be rebuilt, stop speki, delete `<data_dir>/<repository id>/`, and start it again. To
rebuild everything, start with an empty data directory.

Memory: give an instance at least 1.5 GiB. SQLite is allowed up to 1 GiB, which a large query can use
for a moment before it's refused, and Go and the page caches need the rest. An instance serving one
vault of 50,000 documents and ten of 10,000 sat at about 150 MB when measured; it's the peaks the limit is
for. Below about 1.2 GiB, the kernel can kill the process before SQLite's own limit refuses the query.

## Endpoints

| Path | Port | Purpose |
|---|---|---|
| `/mcp` | `listen` | The MCP endpoint |
| `/.well-known/oauth-protected-resource` | `listen` | MCP auth discovery |
| `/.well-known/oauth-authorization-server` | `listen` | OAuth metadata |
| `/oauth/...` | `listen` | Sign-in, consent, token and registration endpoints |
| `/healthz` | `listen` | 200 while the process runs |
| `/readyz` | `listen` | 200 once the first round of vault discovery is done, whether or not it succeeded, or once looking up the app's owner has failed. 503 while starting and stopping. `/mcp` answers 503 at the same times, and to a user whose vaults are all still being cloned or indexed, or failed to open ([access.md](access.md#who-gets-in)) |
| `/metrics` | `metrics_listen` | Prometheus metrics |
| `/status` | `metrics_listen` | Readiness, the store in use (memory or redis), every vault's state, commit, last sync and last error, any `speki.yaml` that couldn't be used, and configuration warnings, as JSON |

Speki runs two separate HTTP servers. The public one (`listen`, default `:8080`) serves only what MCP
clients and browsers need. The internal one (`metrics_listen`, default `:9090`) serves only metrics and the
status page. Neither answers for the other's paths, and speki refuses to start when both are set to the
same address.

In a container, publish only the `listen` port. Prometheus scrapes `metrics_listen` from inside the cluster
or container network, so `/metrics` and `/status` are never reachable from outside, whatever the proxy in
front of speki is configured to forward.

Speki serves plain HTTP. Put it behind something that terminates TLS. `public_url` must be the URL clients
use, because tokens are bound to it.

## Logs, metrics and traces

Logs are JSON on stderr, one line per event, with the request ID, GitHub login, tool, vault and path where
they apply. They never contain tokens, document content or SQL. Errors shown to agents include the request
ID, so a user can hand it to whoever runs speki.

Metrics include tool calls by tool and outcome, tool latency, GitHub requests and remaining rate limit,
per vault whether it's up, its size and how recently it was confirmed current, sync results, and sign-ins.
[operations.md](../design/operations.md) lists them.

Traces are off by default. With `telemetry.tracing: true`, speki sends OpenTelemetry traces to whatever
the standard `OTEL_EXPORTER_OTLP_*` variables point at, with a span per tool call, GitHub request and
index operation.
