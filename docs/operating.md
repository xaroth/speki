# Operating speki

What a running speki needs and shows: ports, disk and memory, how it starts and stops, how to run several
instances, and what to watch. Setting one up is in [running.md](running.md).

## Endpoints

Speki runs two HTTP servers. Neither answers for the other's paths, and speki refuses to start when both are
set to the same address.

| Path | Server | Purpose |
|---|---|---|
| `/mcp` | public (`listen`) | The MCP endpoint |
| `/.well-known/oauth-protected-resource` | public | MCP sign-in discovery |
| `/.well-known/oauth-authorization-server` | public | OAuth metadata |
| `/oauth/...` | public | Sign-in, consent, token and registration endpoints |
| `/healthz` | public | 200 while the process runs |
| `/readyz` | public | 200 once ready (below); 503 while starting and stopping |
| `/metrics` | internal (`metrics_listen`) | Prometheus metrics |
| `/status` | internal | Readiness and every vault's state, as JSON |

Publish only the public port. Prometheus scrapes the internal one from inside the container or cluster
network, so `/metrics` and `/status` are never reachable from outside, whatever the proxy in front forwards.
A separate server, rather than a path rule, means a routing mistake on the public side can't expose them.

Speki serves plain HTTP; put it behind something that terminates TLS. All URLs speki hands out are built
from `public_url`, never from `Host` or `X-Forwarded-*` headers. The proxy's upstream read timeout should be at
least 75 seconds: a request may take 50, and a change may spend longer looking for a commit whose answer was
lost.

## Starting

1. Speki checks the configuration and refuses to start with every problem listed at once.
2. The servers start. `/healthz` answers right away; `/readyz` and `/mcp` answer 503.
3. Leftovers of interrupted index builds are removed. Every vault that has a usable copy and index on disk is
   opened and served as is, without the network.
4. The first round of vault discovery runs, successful or not. Then speki is ready. If the app's owner can't
   be looked up, speki marks itself ready anyway and keeps retrying.
5. Vaults not on disk yet are cloned and indexed in the background, four at a time, and show up once done.
   Polling and, with Redis, change notices run from here on.

So an instance with its data directory intact serves within seconds, even when GitHub is unreachable, and
catches up at its first poll. An instance on an empty disk is ready quickly but has no vaults until the first
ones finish indexing, about a minute for a vault of 50,000 documents.

## Stopping

On `SIGTERM` or `SIGINT`, within about 25 seconds in all:

1. Speki marks itself not ready, for good: `/readyz` and `/mcp` answer 503.
2. Running requests get up to 15 seconds to finish, then their connections are closed.
3. Handlers still running get 3 more seconds.
4. Background work (polling, syncs after commits, change notices) gets 5 seconds.
5. Indexes and the store are closed, unless something still uses them, and traces get 2 seconds to flush.

A second signal ends the process at once. Kubernetes' default 30-second grace period covers this; Docker's
default of 10 seconds doesn't, so set `stop_grace_period: 30s`.

A change still running 15 seconds into a stop loses its connection, but its handler keeps going, so a commit
that was sent lands. The agent sees the connection drop and has to read the document to find out.

## Disk

Speki keeps a shallow git copy and a search index per vault under `sync.data_dir`, in one folder per
repository ID. Rough sizes per 10,000 documents of about 5 KB: 50 MB of git and 150 MB of index. Use local
disk: network file systems make SQLite slow and sometimes break its locking.

The data directory can be wiped at any time, but a persistent volume is better: restarts are fast and don't
need GitHub. To rebuild one vault, stop speki, delete `<data_dir>/<repository id>/`, and start it again. To
rebuild everything, start with an empty data directory.

## Memory

Give an instance at least 1.5 GiB. SQLite may use up to `sqlite.memory_limit_mib` (1 GiB by default), which a
large query can reach for a moment before it's refused, and Go and the page caches need the rest. Below about
1.2 GiB, the kernel can kill the process before SQLite's own limit refuses the query.

Typical use is much lower: an instance serving one vault of 50,000 documents and ten of 10,000 measured about
150 MB. The limit is for the peaks.

Raise `sqlite.query_memory_mib` if agents' queries often fail with `too_large`. Lower it if searches or syncs
fail with out-of-memory errors while queries run.

## One instance or several

Without `redis`, speki runs as a single instance and keeps its short-lived state in memory: used
authorization codes, refresh locks and results, and the access cache. A restart loses that, which only means
some users are checked against GitHub again.

Run exactly one instance without Redis. Several instances with memory stores don't share refresh locks: when a
client's refreshes land on two of them, both use the same single-use GitHub refresh token, and the user is
signed out. Speki logs a warning at startup when it uses the memory store, and `/status` shows it.

With `redis`, any number of instances can run behind a load balancer, without sticky sessions. They share that
state through Redis, and when one writes to a vault, the others hear about it and sync within seconds instead
of at the next poll. Every instance needs the same configuration: the same `public_url`, GitHub App, keys and
Redis.

- Speki needs Redis 6.2 or later, or Valkey. An instance that can't reach Redis at startup refuses to start.
- If Redis goes away later, sign-in and refresh fail until it's back, every request checks access with GitHub,
  and changes reach other instances at their next poll.
- Configure Redis not to evict keys (`maxmemory-policy noeviction`). Every speki key has a time to live, so
  policies like `volatile-ttl` would evict exactly the used-code markers, and an evicted one could let an
  authorization code be used twice. Persistence isn't needed: all keys live minutes at most, prefixed
  `speki:`.
- The per-user rate limit (10 requests a second, bursts of 30) is kept by each instance. With N instances, a
  user can make up to N times as many requests.
- Every instance clones and indexes every vault itself, into its own data directory. Don't share one data
  volume between instances. Plan disk and memory per instance.

With the Compose setup from [running.md](running.md), add a Redis service and point speki at it:

```sh
printf '%s' 'redis://redis:6379/0' > secrets/redis-url
sudo chown 65532 secrets/redis-url && chmod 400 secrets/redis-url
```

In `docker-compose.yaml`, add the service and the secret, and give the secret to speki:

```yaml
services:
  redis:
    image: redis:7-alpine
    command: ["redis-server", "--maxmemory-policy", "noeviction", "--save", "", "--appendonly", "no"]
    restart: unless-stopped

  speki:
    # ...as before, plus:
    secrets:
      - github-app-pem
      - github-client-secret
      - speki-keys
      - redis-url

secrets:
  # ...as before, plus:
  redis-url:
    file: ./secrets/redis-url
```

In `config.yaml`:

```yaml
redis:
  url_file: /run/secrets/redis-url
```

To run more than one instance, each needs its own data volume, so define one speki service per instance
rather than scaling one service that shares `speki-data`.

## Status page

`GET /status` on the internal port returns JSON:

- `ready`: what `/readyz` reports.
- `store`: `memory` or `redis`.
- `vaults`: every vault with its state (`serving`, `building`, `failed`), index commit, document count and
  size, a warning when it's larger than speki is built for, last successful sync, when the index was last
  confirmed current, and last error.
- `problems`: repositories whose `speki.yaml` couldn't be used, with the reason.
- `warnings`: configuration warnings, such as the memory store.

It's the first place to look when someone says their vault isn't showing up.

## Logs

JSON on stderr, one line per event. Every request has a request ID, taken from an incoming `X-Request-ID` when
it's a plain token of at most 64 characters, otherwise generated. It's in every log line, in errors shown to
agents, and on traces, so a user can hand it to whoever runs speki.

Every tool call logs one line at the end with the request ID, GitHub login, client, tool, vault, path, outcome
and duration. Background work logs discovery changes, sync results, index builds and failures, per vault.

Logs never contain tokens, codes, cookies, document content or SQL. As a safety net under that rule, attributes
with names like `token`, `code`, `content` or `sql` are redacted.

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `speki_tool_calls_total` | counter | `tool`, `outcome` (`ok`, an error code, `timeout`, `canceled`) |
| `speki_tool_duration_seconds` | histogram | `tool` |
| `speki_github_requests_total` | counter | `kind` (app, installation, user, oauth), `status` |
| `speki_github_rate_remaining` | gauge | `kind`. For installations, the lowest across installations heard from recently |
| `speki_vault_up` | gauge | `vault`. 1 when served and its last sync succeeded |
| `speki_vault_documents` | gauge | `vault` |
| `speki_vault_bytes` | gauge | `vault`. Markdown bytes indexed |
| `speki_vault_fresh_timestamp_seconds` | gauge | `vault`. When speki last confirmed the index matches GitHub |
| `speki_sync_total` | counter | `result` (unchanged, applied, rebuilt, deferred, failed) |
| `speki_write_guard_mismatches_total` | counter | Changes refused because the index and git disagreed on a document: an index bug ([changes.md](changes.md#versions-and-conflicts)) |
| `speki_signins_total` | counter | `result` |
| `speki_refreshes_total` | counter | `result` (ok, shared, unreached, no_answer, app_config, signed_out, lost_to_timeout) |

Plus Go and process metrics. User logins are never labels.

Alert on `time() - speki_vault_fresh_timestamp_seconds`: above a few poll intervals, a vault is falling behind.
It's a timestamp rather than an age so it keeps growing when the poller itself stalls.

Speki warns in the logs when a GitHub rate limit drops below 10%. Its own background use is light: polling 20
vaults every minute is about 1,200 requests an hour, nearly all free conditional requests. Per-user calls
(access checks, commits, history) use the user's own GitHub rate limit.

## Traces

Off by default. With `telemetry.tracing: true`, speki sends OpenTelemetry traces over OTLP/HTTP to wherever
the standard `OTEL_EXPORTER_OTLP_*` variables point. There's a span per request, tool call, GitHub request,
search, query, index update and sync. Spans follow the logging rules, and leave out query strings, because the
GitHub sign-in callback carries its code there. Speki doesn't send its trace context to GitHub.

## When something's wrong

| Symptom | Look at |
|---|---|
| A vault doesn't show up | `/status`: is it under `problems` (a bad `speki.yaml`), or `failed`? Is the app installed on the repository by an allowed account? Discovery runs every 5 minutes |
| A vault shows old content | `/status` for its last sync and error; `speki_vault_fresh_timestamp_seconds` |
| Users get 403 | Not in a required organization, or no readable vault. The message says which |
| Users are signed out often | `speki_refreshes_total` by result. `lost_to_timeout` means GitHub timeouts; several instances on the memory store cause sign-outs too |
| Agents get `unavailable` | Find the request ID from the error in the logs |
| Agents get `vault_read_only` | The default branch requires pull requests or checks |
| Speki starts but never serves vaults | `sync.data_dir` not writable; check the logs at startup |
