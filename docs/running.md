# Running speki

Speki is one binary. It needs a GitHub App, a configuration file, a secret key, some local disk it may lose
at any time, and something in front of it that terminates TLS. Running more than one instance also needs
Redis or Valkey ([operating.md](operating.md#one-instance-or-several)).

This page walks through a first instance from nothing, with Docker Compose, then covers configuration, keys
and building in detail.

## 1. Register the GitHub App

Create a GitHub App in the account or organization that runs speki: Settings, Developer settings, GitHub
Apps, New GitHub App.

| Setting | Value |
|---|---|
| Homepage URL | anything, for example your speki's URL |
| Callback URL | `https://<speki host>/oauth/github/callback` |
| Expire user authorization tokens | on. Speki refuses tokens that don't expire |
| Request user authorization (OAuth) during installation | off |
| Enable Device Flow | off |
| Webhook | off. Speki polls; it has no webhook endpoint |
| Repository permissions | Contents: read and write. Metadata: read |
| Organization permissions | Members: read |
| Where can this GitHub App be installed | Only on this account (recommended) |

Don't grant anything else. Without the Workflows permission, GitHub itself refuses writes to
`.github/workflows/`, whatever speki does.

Then, on the app's settings page:

1. Note the App ID and the Client ID.
2. Generate a client secret and save it.
3. Generate a private key and save the downloaded `.pem` file.

Install the app (the app's page, Install App) on the repositories that will be vaults. If you'll require
organization membership, or name teams in a vault's `readers` or `writers`, install the app on each of those
organizations too, with no repositories selected if it holds no vaults: GitHub only answers membership
questions for organizations where the app is installed.

By default speki only counts installations owned by the account that owns the app. To allow other accounts,
list their numeric IDs in `github.allowed_accounts`. `gh api users/<name> --jq .id` gives an account's ID.

## 2. Create the secrets

Make a folder for the deployment with a `secrets/` folder in it:

```sh
mkdir -p speki/secrets && cd speki
cp ~/Downloads/<app-name>.*.private-key.pem secrets/github-app.pem
printf '%s' '<client secret>' > secrets/github-client-secret
```

Generate the key speki seals its tokens with:

```sh
printf 'keys:\n  - id: "%s"\n    secret: "%s"\n    primary: true\n' \
  "$(date +%Y-%m)" "$(openssl rand -base64 32)" > secrets/speki-keys.yaml
```

The container runs as user ID 65532, which must be able to read the secrets:

```sh
chmod 400 secrets/*
sudo chown 65532 secrets/*
```

## 3. Write the configuration

`config.yaml`:

```yaml
public_url: https://speki.example.com

github:
  app_id: 123456
  client_id: Iv23liAbCdEf
  private_key_file: /run/secrets/github-app.pem
  client_secret_file: /run/secrets/github-client-secret
  # allowed_accounts: [87654321]

# access:
#   required_orgs: [acme]

auth:
  keys_file: /run/secrets/speki-keys.yaml

sync:
  # Required in the image: the default, "data" in the working directory, isn't writable there.
  data_dir: /var/lib/speki

# instance:
#   name: work
#   title: Acme work knowledge
#   description: Documentation for Acme projects and services, architecture, runbooks and decisions.
```

`public_url` is the address clients use. Tokens are bound to it, and it must be `https`.

## 4. Start it with Docker Compose

Speki serves plain HTTP, so this setup puts Caddy in front for TLS. Caddy gets a certificate for the host
in the `Caddyfile` by itself, as long as DNS points at the machine and ports 80 and 443 are reachable.

`docker-compose.yaml`:

```yaml
services:
  # Makes the data volume writable for the image's non-root user.
  speki-init:
    image: busybox:1.36
    command: ["chown", "-R", "65532:65532", "/var/lib/speki"]
    volumes:
      - speki-data:/var/lib/speki

  speki:
    image: ghcr.io/xaroth/speki:latest   # pin a version, such as 1.2, in production
    depends_on:
      speki-init:
        condition: service_completed_successfully
    volumes:
      - ./config.yaml:/etc/speki/config.yaml:ro
      - speki-data:/var/lib/speki
    secrets:
      - github-app.pem
      - github-client-secret
      - speki-keys.yaml
    # Environment variables override config.yaml:
    # environment:
    #   SPEKI_TELEMETRY_LOG_LEVEL: debug
    expose:
      - "8080"   # public: MCP and sign-in, reached through Caddy
      - "9090"   # internal: metrics and status; never publish it
    mem_limit: 1536m
    # Speki takes up to about 25 seconds to stop cleanly; Docker's default is 10.
    stop_grace_period: 30s
    restart: unless-stopped

  caddy:
    image: caddy:2
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
    depends_on:
      - speki
    restart: unless-stopped

secrets:
  github-app.pem:
    file: ./secrets/github-app.pem
  github-client-secret:
    file: ./secrets/github-client-secret
  speki-keys.yaml:
    file: ./secrets/speki-keys.yaml

volumes:
  speki-data:
  caddy-data:
```

`Caddyfile`:

```text
speki.example.com {
	reverse_proxy speki:8080 {
		health_uri /readyz
		transport http {
			read_timeout 90s
		}
	}
}
```

The read timeout is above speki's own 75-second write timeout, so a slow change isn't cut off by the proxy.

Start it:

```sh
docker compose up -d
docker compose logs -f speki
```

Some things to know about the image:

- It's distroless: there is no shell, `curl` or `wget` inside, so a Docker `healthcheck` can't run. Probe
  `/readyz` from outside, as Caddy does above.
- A new named volume is owned by root, which is why `speki-init` hands it to user 65532. With a bind mount
  instead, `chown 65532` the host folder.
- Without `sync.data_dir: /var/lib/speki`, speki starts and reports ready but can't store any vault.
- The published image is a multi-platform image for linux/amd64 and linux/arm64, signed with cosign
  ([Building](#building)). Both base images are pinned by digest; Dependabot proposes updates weekly, and a
  merged update reaches the image with the next release.

## 5. Check that it runs

```sh
curl https://speki.example.com/readyz
docker compose exec caddy wget -qO- http://speki:9090/status
```

`/readyz` answers 200 once speki has finished its first look for vaults. `/status`, on the internal port,
lists every vault with its state, any `speki.yaml` it couldn't use, and configuration warnings
([operating.md](operating.md#status-page)). Right now it has no vaults.

## 6. Make a vault

Add a `speki.yaml` to the default branch of a repository the app is installed on:

```yaml
title: Infrastructure knowledge
description: How our services are deployed and operated, and why.
```

Speki finds it within 5 minutes (`sync.discovery_interval`; with several instances sharing Redis, an instance
may take up to about twice that) and serves it once it's cloned and indexed, usually within a minute after
that. [vaults.md](vaults.md) has every option.

## 7. Connect a client

The MCP endpoint is `<public_url>/mcp`.

Claude Code:

```sh
claude mcp add --transport http speki https://speki.example.com/mcp
```

With `instance.name` set, use `speki-<name>` as the name, so permission rules can tell instances apart
([access.md](access.md#using-more-than-one-speki)).

claude.ai and the Claude apps: add a custom connector with the URL `https://speki.example.com/mcp`, and no
client ID or secret.

The first use opens a browser to sign in ([access.md](access.md#signing-in)). Other hosted web clients need
their host in `auth.client_metadata_hosts`; local programs register themselves.

## Running without Docker

Build the binary (below) and start it with `bin/speki --config /path/to/config.yaml`. Without `--config`,
speki reads the file named by `SPEKI_CONFIG`, then `config.yaml` in the working directory if there is one, and
otherwise runs on environment variables alone. `speki --version` prints the version. There are no other flags
or subcommands.

For local development, `public_url` may be `http://localhost:<port>` or `http://127.0.0.1:<port>`.

## Configuration

Speki reads the YAML file, then environment variables, which win. The file is decoded strictly: an unknown
key fails with its line number, so a typo doesn't go unnoticed. Speki refuses to start when something
required is missing or a value is invalid, and lists every problem at once. YAML anchors, aliases and merge
keys (`<<`) aren't supported in the file.

Every key has an environment variable: `SPEKI_` plus the key's path in upper case with `_` between parts, so
`telemetry.log_level` is `SPEKI_TELEMETRY_LOG_LEVEL`. Lists are comma-separated. Durations use Go syntax
(`60s`, `5m`).

Any text setting can come from a file: `private_key_file` in YAML, `SPEKI_GITHUB_PRIVATE_KEY_FILE` in the
environment. Use files for secrets: they don't show up in process listings or crash dumps. A setting and its
`_file` form can't both be set. An environment variable wins over a `_file` key in the YAML, which then isn't
read, so a config file baked into an image can be overridden at deploy time.

| Key | Default | Meaning |
|---|---|---|
| `public_url` | required | The URL clients use. `https`, no path. Tokens are bound to it |
| `listen` | `:8080` | Public server: MCP, sign-in, health checks |
| `metrics_listen` | `:9090` | Internal server: metrics and status. Must differ from `listen` |
| `github.app_id` | required | The GitHub App's ID |
| `github.private_key` | required | The app's private key, PEM. Usually `private_key_file` |
| `github.client_id` | required | The app's client ID |
| `github.client_secret` | required | The app's client secret. Usually `client_secret_file` |
| `github.allowed_accounts` | the app's owner | Numeric IDs of the accounts whose installations count |
| `access.required_orgs` | none | Users must be an active member of at least one of these organizations |
| `auth.key` | | A single key, standard base64 of at least 32 random bytes |
| `auth.keys` | | The contents of a key file, usually given as `keys_file`. Exactly one of `key` and `keys` |
| `auth.client_metadata_hosts` | `claude.ai` | Hosts whose client metadata documents are accepted, as bare lowercase host names (`claude.ai`, not `https://claude.ai`). Empty means the default |
| `sync.data_dir` | `data` | Git copies and search indexes. Local disk |
| `sync.poll_interval` | `1m` | How often to check vaults for new commits |
| `sync.discovery_interval` | `5m` | How often to look for new and removed vaults |
| `redis.url` | none | `redis://` or `rediss://`, password included. Needed for more than one instance. Usually `url_file` |
| `telemetry.log_level` | `info` | `debug`, `info`, `warn` or `error` |
| `telemetry.tracing` | false | Send OpenTelemetry traces where the `OTEL_EXPORTER_OTLP_*` variables say |
| `instance.name` | none | Lowercase letters, digits and dashes, starting with a letter or digit, at most 32. The server is called `speki-<name>` |
| `instance.title` | none | One line, at most 100 characters. Shown on the sign-in page and in `guide` |
| `instance.description` | none | What belongs in this speki, at most 300 characters. Opens the agents' instructions |
| `sqlite.memory_limit_mib` | `1024` | SQLite's memory limit for the whole process. 128 to 1048576 |
| `sqlite.query_memory_mib` | `384` | Where agents' SQL queries are stopped. At most 3/4 of the limit |

Without `instance`, speki calls itself `speki` and describes itself as the team's knowledge base. Set it when
people may connect to more than one speki, so their agents know which one to search and where to write.

`config.example.yaml` in the repository has every key an operator normally sets, with comments.
`github.api_url`, `github.git_url` and `github.oauth_url` point speki at another GitHub (defaults
`https://api.github.com/`, `https://github.com` and `https://github.com`) and are meant for tests.

## Keys

Speki seals its tokens with a key. Set exactly one of:

- `auth.key`: a single key. Simplest to set from a secret store (`SPEKI_AUTH_KEY_FILE`), but it can't be
  rotated without first moving to a key file.
- `auth.keys`: a key file, which can hold several keys and supports rotation.

```yaml
keys:
  - id: "2026-10"
    secret: "<base64 of 32 random bytes>"
    primary: true
  - id: "2026-04"
    secret: "..."
```

IDs must be unique and have no dots, and exactly one key is primary. New tokens are sealed with the primary
key; any listed key opens existing ones. Every instance needs the same key file.

To rotate:

1. Add a new key, not primary, on every instance. Restart.
2. Make it primary. Restart.
3. Remove the old key after 90 days, when the last refresh token sealed with it has expired.

Removing a key early signs out everyone who still holds tokens sealed with it. Removing a key at any time
invalidates the client IDs of local programs that registered themselves while it was primary, because those
IDs don't expire. Such programs have to register again; some keep the old ID until it's cleared in the
client.

If a key may have leaked, remove it and also revoke all user tokens in the GitHub App's settings: someone with
the key could open captured tokens and use the GitHub token inside.

To move from `auth.key` to a key file, list the same secret under the ID `default`, which is the ID speki gives
the single key, so existing tokens keep opening:

```yaml
keys:
  - id: default
    secret: "<the auth.key value>"
    primary: true
```

Replace `auth.key` with `auth.keys` on every instance and restart. Then rotate as above. Listing the old key
under any other ID signs everyone out.

## Building

The search index uses SQLite through cgo, so a build needs Go and a C compiler. `go.mod` pins the Go
toolchain; with the default `GOTOOLCHAIN=auto`, an older local Go downloads it. The build tags enable
SQLite's full-text search (`sqlite_fts5`) and leave out extension loading (`sqlite_omit_load_extension`). The
build fails without either, so a binary can never link SQLite's extension loading.

```sh
task build    # writes bin/speki
```

Without [Task](https://taskfile.dev):

```sh
CGO_ENABLED=1 go build -tags 'sqlite_fts5,sqlite_omit_load_extension' \
  -ldflags "-X main.version=1.0.0" -o bin/speki ./cmd/speki
```

The Dockerfile builds the same binary into a distroless image that runs as a non-root user:

```sh
docker build --build-arg VERSION=1.0.0 -t speki .
```

Publishing a GitHub release first runs CI (lint and tests) on the tagged commit. It then builds the image for
linux/amd64 and linux/arm64 and pushes it to `ghcr.io/xaroth/speki`, tagged with its version (`1.2.3`).
Releases that aren't prereleases also get the minor version (`1.2`) and move `latest`. Each platform image
carries an SBOM and build provenance, and the multi-platform image is signed with cosign keyless signing. To
check that an image came from this repository's release workflow:

```sh
cosign verify ghcr.io/xaroth/speki:1.2.3 \
  --certificate-identity-regexp '^https://github.com/xaroth/speki/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect ghcr.io/xaroth/speki:1.2.3 --format '{{json .Provenance}}'
docker buildx imagetools inspect ghcr.io/xaroth/speki:1.2.3 --format '{{json .SBOM}}'
```

Other tasks: `task test` (with the race detector), `task lint`, `task vet`, `task vuln`, `task vuln:image`
(scans the binary in a published image and fails on vulnerabilities that have a fix), `task fuzz`, `task run`
(builds and runs with `./config.yaml`), and `task ci`, which runs what CI runs on a pull request.
`govulncheck` runs weekly on the source and on the published image; a failure on the image means a release
is due.
