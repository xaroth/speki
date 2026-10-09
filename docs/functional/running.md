# Running speki

Speki is one binary. It needs a GitHub App, a configuration file, a secret key and some disk it may lose at
any time. For more than one instance it also needs Redis or Valkey.

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

Speki reads a YAML file (`--config`, default `config.yaml` in the working directory) and then environment
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
  allowed_accounts: [87654321]   # eve-online-tools

access:
  # Users must be an active member of at least one. Empty means no org check.
  required_orgs: [eve-online-tools]

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
```

Every key has an environment variable: `SPEKI_` plus the path in upper case with `_` between parts.
`github.client_secret_file` becomes `SPEKI_GITHUB_CLIENT_SECRET_FILE`. Secrets can be given directly
(`SPEKI_GITHUB_CLIENT_SECRET`) or as a file path (`..._FILE`). Files are better: they don't show up in
process listings or crash dumps.

Speki refuses to start when something required is missing or a value is invalid, and says which key.

### The key file

Speki seals its tokens with keys from this file:

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

`openssl rand -base64 32` makes a good secret.

## One instance or several

With no `redis` configured, speki runs as a single instance and keeps its short-lived state in memory:
used authorization codes, refresh locks, the per-user access cache. A restart loses that state, which only
means a few users get checked against GitHub again.

With `redis` configured, any number of instances can run behind a load balancer without sticky sessions.
They share that state through Redis, and when one instance writes to a vault, it tells the others, so
they pick up the change within seconds instead of at the next poll.

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

A vault that fails to index (too big, bad `speki.yaml`, GitHub unreachable) is logged, shown on the status
page, and retried at the next discovery round. The other vaults aren't affected.

To force a vault to be rebuilt, stop speki, delete `<data_dir>/<repository id>/`, and start it again. To
rebuild everything, start with an empty data directory.

## Endpoints

| Path | Port | Purpose |
|---|---|---|
| `/mcp` | `listen` | The MCP endpoint |
| `/.well-known/oauth-protected-resource` | `listen` | MCP auth discovery |
| `/.well-known/oauth-authorization-server` | `listen` | OAuth metadata |
| `/oauth/...` | `listen` | Sign-in, consent, token and registration endpoints |
| `/healthz` | `listen` | 200 while the process runs |
| `/readyz` | `listen` | 200 once the first indexing round has finished |
| `/metrics` | `metrics_listen` | Prometheus metrics. Keep this port private |
| `/status` | `metrics_listen` | Every vault's state, commit, last sync and last error, as JSON |

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
