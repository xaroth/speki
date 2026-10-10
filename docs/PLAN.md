# Build plan

How speki gets built, in the order that gets a useful, safe version in front of people soonest. Each
milestone ends with something that runs and is tested, and is merged to `main` before the next starts.

The design documents describe the end state. This plan only says in which order to get there and what
"done" means for each step.

## Order at a glance

| # | Milestone | Ends with |
|---|---|---|
| 0 | Foundations and checks | A skeleton that builds with cgo, and answers to the open questions |
| 1 | Documents | The `document` package, complete |
| 2 | Vaults and git copies | Speki finds vaults and keeps local copies of them current |
| 3 | Index and search | Searchable vaults, measured |
| 4 | Sign-in and access | Claude can sign in, and every request carries a `Grant` |
| 5 | Read tools | First internal release: search and read from Claude |
| 6 | Write tools | Agents can write back |
| 7 | Query | Agent SQL in the sandbox |
| 8 | Several instances | Redis, change notices, operations polish |
| 10 | Proven against reality | The open checks done by hand, the team using it, someone else's review |

Milestones 1 and 2 can run in parallel, as can 3 and 4. Everything else builds on what came before.
Milestone 10 picks up the parts of earlier milestones that only a real GitHub App, real clients, other
people and a second instance can finish.

## 0. Foundations and checks

Build:

- `go.mod` (`github.com/eve-online-tools/speki`, Go 1.27).
- `cmd/speki`: `main.go` with signal handling and `run`, `config.go` with the file, environment and
  `_file` secret loading. Tests for both.
- `service`: `Config` with defaults, validation and the `Secret` type; `New` and `Run` with `/healthz`
  and graceful shutdown. A first test that starts and stops a `Service`.
- `telemetry`: the slog handler with redaction and request IDs. Metrics registry and tracing come later,
  with the code they measure.
- `Taskfile.yml`, CI (vet, staticcheck, `go test -race`, govulncheck, all with the cgo build tags), and the
  two-stage Dockerfile.
- [CONVENTIONS.md](CONVENTIONS.md) is in place before the first package lands.

Check, each with a small throwaway program, and write the results into the research notes:

1. GitHub writes with a user token through `createCommitOnBranch` on a scratch repository: does push
   protection reject a test secret, what do branch rulesets (required PR, required checks) return, is the
   commit authored by the user and verified? This decides the write API in `gh`.
2. GitHub reads with a user token: `GET /user/installations/{id}/repositories` returns per-repository
   permissions; `GET /user/memberships/orgs/{org}` works with the app's `Members: read`; a conditional
   `GET .../git/ref/heads/{branch}` returns 304.
3. SQLite with mattn: `ATTACH ... mode=ro` of a WAL database another connection is writing to;
   `fts.body MATCH` through a `UNION ALL` view across attached databases; the authorizer calls FTS5 makes
   (shadow tables, `data_version`); the `auto_extension` hook enabling `DEFENSIVE` only for `:memory:`
   connections; soft heap limit behaviour.
4. go-git: fetch with depth 1 into an existing shallow repository, then diff the old and new trees.
   Repeat 100 times and look at pack files and timing.
5. go-sdk with `Stateless: true` and a dummy tool, reached from Claude Code and from claude.ai as a custom
   connector. Does claude.ai use a client metadata document when it's advertised, which host serves it, and
   does it send an `Origin` header? Do the server instructions show up?

Done when: CI is green on the skeleton, the image builds and runs, and each check has a written answer.
Where an answer contradicts the design, the design document is updated in the same change.

## 1. Documents

Build `document`: frontmatter split and parsing, core fields with warnings, `Check`, `Touch` with
node-tree editing and line-ending preservation, goldmark parsing for headings and links, wikilinks,
link candidates, `CheckPath`.

Tests: table-driven, with fixtures in `testdata/` taken from real Obsidian vaults and GitHub-style
documents, including the ugly ones (CRLF, BOM, no frontmatter, broken YAML, anchors, tags as a string,
NFD file names). A fuzz test for `Parse` and `Touch`: parsing never panics, and touching a document twice
gives the same bytes as touching it once.

Done when: every rule in [documents.md](functional/documents.md) and [document.md](design/document.md) has a
test.

## 2. Vaults and git copies

Build:

- `gh`: app JWT and installation tokens (ghinstallation), listing installations and repositories, fetching
  `speki.yaml`, conditional ref lookups, rate limit metrics. Tested against recorded responses.
- `vault`: `speki.yaml` parsing with limits, the registry, the discovery loop with its removal rules.
- `gitsync`: open or clone, fetch, diff, files, blob, eligibility rules, broken-mirror detection.
  Tested against local bare repositories.

Done when: pointed at a test organisation (manually) or at fakes (in CI), speki discovers vaults, clones
them, notices a push within a poll interval, and logs what it did. Removing `speki.yaml` removes the vault.
A failing API call doesn't.

## 3. Index and search

Build:

- `index`: schema, `Open` with generations and cleanup, `Build`, `Apply` with per-file error handling,
  vocabulary loading, `Search` with query parsing, correction, prefix and substring hits, `List`,
  `Document`, `LinkedFrom`.
- `library`: per-vault copy, sync lock, sync from the index's commit, the poll loop, the status
  data behind `/status`.
- A fixture corpus generator: realistic markdown with frontmatter, 10,000 and 50,000 documents.

Measure on the fixture corpus, and write the numbers into [the search research](research/search-engine.md):
full build time, `Apply` time for one document, search latency (p50, p95) across 1 and 10 vaults, index
size, memory. Starting targets: search p95 under 100 ms across 10 vaults of 10,000 documents on 4 cores,
build under 2 minutes for 50,000 documents. If a target is missed, find out why before moving on.

Done when: the examples in [search-and-query.md](functional/search-and-query.md) (stemming, typos, prefixes,
substrings, phrases, exclusions, deprecated ranking) pass as tests, and the numbers are written down.

## 4. Sign-in and access

Build:

- `seal`: keyring loading, sealing and opening, purposes, skew.
- `store`: the interfaces' in-memory implementation (single-use keys, locks, short-lived values).
- `oauth`: metadata endpoints, registration, client metadata documents with the restricted fetcher,
  authorize and consent page, GitHub callback, token endpoint with code and refresh grants, the token check.
- `access`: the `Grant`, the resolver with org gate and vault permissions, the cache keyed by token hash.
- `mcpserver.Handler` with the full middleware chain and an empty tool list.

Tests: each OAuth step's failure cases (wrong redirect, missing PKCE, replayed code, wrong resource,
expired values, cookie mismatch, client metadata from a host not on the list, a metadata URL resolving to
a private address), refresh races, read-only scope producing a `Grant` without writable vaults.

Then by hand, with a GitHub App registered for testing: sign in from Claude Code and from claude.ai, both
read-write and read-only, and as a user outside the required organisation.

Done when: both clients sign in, refresh after an hour without user action, and a user outside the
organisation is turned away with a clear page.

## 5. Read tools, first internal release

Build:

- `knowledge`: list, search (fan-out and merge), read (from the mirror, with line ranges, outline and the
  byte cap), history, guide content. Every function takes a `Grant`.
- `mcpserver`: the instructions, `guide.md`, and the tools `guide`, `list`, `search`, `read`, `history`,
  with descriptions, annotations, text renderings and error mapping.
- Metrics for tool calls and vaults. `/status`.
- The end-to-end test in `service`: fake GitHub, local repositories, the go-sdk client, signing in and
  searching.

Release: deploy one instance for the team, with a persistent data directory, and a few real vaults. Use it
from Claude for a week before writing is turned on. Watch whether agents reach for speki without being
told, and tune the instructions and tool descriptions based on what they actually do.

Done when: the team uses it, and nobody has seen another vault's content (there's a test for every read
path that tries).

## 6. Write tools

Build:

- `gh.Commit` with the API chosen in milestone 0, and the error mapping.
- `knowledge`: write, edit, move, delete, with version checks, the per-vault write lock, the existence and
  collision check against the mirror (with `gitsync` `Paths`), the retry on a moved head, message
  sanitising, the post-write sync and `index_pending`, `linked_from`.
- Change notices: the call site after a commit, with a no-op on the in-memory store. (Built with the
  Redis store in milestone 8, call site included.)

Tests: concurrent edits to one document and to different documents of one vault; a write to a path that
exists in git but not in the index; case and normalisation collisions; frontmatter with existing problems;
a read-only session; branch rules and push protection errors (recorded responses from milestone 0).

Done when: an agent in Claude can find a document, edit it, and find the change by searching right after,
and every error code in [tools.md](functional/tools.md) has a test that produces it.

## 7. Query

Build: the sandbox in `index`, `schema`, `query`, the one-query-per-user rule, error scrubbing.

Tests: a suite of escape attempts that must all fail (every denied action, `sqlite_master`, another
vault's file, `load_extension`, recursive CTEs, cross joins, huge strings, multiple statements, comments
hiding a second statement), and the examples from [search-and-query.md](functional/search-and-query.md),
which must all work.

Done when: both suites pass, and a security review of `index`'s sandbox code by someone who didn't write it
finds nothing open.

## 8. Several instances

Build:

- `store` on Redis: single-use codes, refresh locks and results, change notices over pub/sub.
- Tracing with OpenTelemetry. Remaining metrics.
- The runbook in [running.md](functional/running.md) checked against reality: key rotation, forced
  rebuild, a leaked key, a vault that won't index.

Test with two instances behind a load balancer: sign in on one, use the other; write on one, search on the
other within seconds; kill one in the middle of a write; refresh the same token on both at once.

Done when: those scenarios behave as described, and an operator who didn't build speki can follow the
runbook to rotate a key.

## 10. Proven against reality

Milestones 0 to 8 are built and pass their tests against fakes. Their "done when" still needs things CI
can't provide: a registered GitHub App, Claude Code and claude.ai as clients, the team, a reviewer who
didn't write the code, and two instances behind a load balancer. This milestone does those, in order. It
adds no features. Code changes only fix what a step finds, and each fix comes with a test that fails
without it. Refactors that don't serve a step wait until the milestone is done.

Each step gets a GitHub issue that holds its results, and closes when its "done when" holds.

### 10.1 Test setup

- A test organisation with the GitHub App registered as in [running.md](functional/running.md), and
  three scratch vaults: one plain, one with a ruleset that requires a pull request, one with a ruleset that
  requires a status check. Push protection on for all three.
- A second GitHub account outside the organisation, and one with only read access to one vault.
- A staging instance with a public HTTPS address, its own keys, and Redis. It runs the image CI builds
  from `main`.

Done when: the staging instance is ready and lists the three vaults on `/status`.

### 10.2 GitHub checks

Milestone 0 checks 1 and 2, against the scratch vaults, through speki's own `gh` package where it can be:

1. `createCommitOnBranch` with a test secret (a revoked token of a provider push protection knows) in a
   new file and in an edit. Is it rejected, with which status and body?
2. The same mutation against the pull-request and status-check rulesets. Which errors come back?
3. The commit's author and verification badge when written with a user token.
4. `GET /user/installations/{id}/repositories` for the read-only user: are the per-repository
   permissions there and correct? `GET /user/memberships/orgs/{org}` for a member, a pending invite, and
   the outside account.
5. A conditional ref lookup that returns 304, and whether it counts against the rate limit.

Record every response under `gh/testdata/` and test `gh` against them, so the error mapping matches
GitHub's real bodies instead of loose message matches. If push protection doesn't cover
`createCommitOnBranch`, switch `gh.Commit` to the Git Data API, as [git-access.md](research/git-access.md)
allows.

Done when: each question has its answer in [git-access.md](research/git-access.md) or
[security.md](research/security.md), the "not yet verified" notes are gone, and `secret_detected`,
`vault_read_only` and the other write errors in [tools.md](functional/tools.md) each have a test fed
by a recorded response.

### 10.3 Clients

Milestone 0 check 5 and milestone 4's manual tests, against staging:

1. Sign in from Claude Code and from claude.ai, read-write and read-only.
2. Sign in as the outside account: a clear page, no token.
3. Leave both clients for over an hour, then use them: they refresh without asking.
4. claude.ai: does it use the client metadata document, from which host, and does it send `Origin`?
   Do the server instructions reach the model, in both clients?
5. A read-only session tries to write and gets `read_only_session`; the read-only user can't write to
   the vault they only read.

Done when: every case behaves as [access.md](functional/access.md) says, and the answers are in
[mcp-protocol.md](research/mcp-protocol.md) and [authentication.md](research/authentication.md).

### 10.4 Sandbox review

Milestone 7's last condition. Someone who didn't write `index/sandbox.go` reviews it with
[index.md](design/index.md) and [security.md](research/security.md) in hand, and tries to break it on
staging through `query`. Scope: the authorizer, attached databases, limits (time, memory, result size),
error scrubbing, and reaching another vault's rows or the file system.

Each finding is an issue. Each fix adds the attempt to `index/sandbox_test.go`.

Done when: the reviewer signs off that nothing is open.

### 10.5 Read-only release

Milestone 5's release, which was skipped. One production instance with a persistent data directory and
the team's real vaults. Everyone signs in read-only for the first week.

Watch, and write down at the end of the week:

- Whether agents reach for speki without being told, from the tool call metrics and from asking people.
- Searches that found nothing, or found the wrong thing.
- Errors, slow calls, and memory and disk use against [running.md](functional/running.md).

Tune the instructions and tool descriptions on what agents actually did.

Done when: the team has used it for a week, the notes are written, and nobody reports seeing a vault they
can't read on GitHub.

### 10.6 Writes on

People sign in read-write. Agents edit, move and delete in real vaults for a week.

Done when: milestone 6's condition holds for real (an agent finds a document, edits it, and finds the
change by searching right after) and no write was lost, applied twice, or landed in the wrong place.

### 10.7 Several instances and the runbook

Milestone 8's tests, on staging with two instances behind a load balancer:

1. Sign in on one, use the other.
2. Write on one, search on the other within seconds.
3. Kill one in the middle of a write: the write either lands once or not at all, and the agent is told
   which.
4. Refresh the same token on both at once: one new token pair, both callers get it.

Then someone who didn't build speki follows [running.md](functional/running.md) on staging: key rotation, a
forced rebuild, a leaked key, a vault that won't index. Every place they get stuck is fixed in the runbook.

Done when: the four scenarios behave as described, and the operator finished the runbook without help.

### 10.8 Housekeeping

- `toolchain` in `go.mod` pinned to the newest Go patch release, and `govulncheck` in pull request CI and
  weekly, so Go security releases are noticed when they ship.
- The README says what speki is now.
- Issue #25 done or closed with what's left.

Done when: CI runs `govulncheck` and it passes.

Done when (milestone): every step above is done, and the team has used speki for writing for a week.
