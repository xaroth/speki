# Code conventions

Rules for Go code in this repo. Review against them, and change this file when a rule changes. The
[go-skills guide](https://github.com/spf13/go-skills/blob/main/go/SKILL.md) covers everything not
written down here.

## Packages

- One module, flat. Packages sit at the top level and are named after what they do (`vault`, `index`),
  never after a layer (`repository`, `handlers`) or a grab bag (`util`, `common`). No `internal/`.
- The command is `cmd/speki`. It's the only code that reads flags, the environment, files named on the
  command line, or signals. Everything it learns goes into `service.Config`.
- `service` is the composition root: it builds and runs everything, and nothing imports it except
  `cmd/speki`.
- A package's exported API is its contract with the rest of speki. Keep it small, and unexport helpers.
- A package that needs something from another declares an interface for exactly what it uses, in the
  file that uses it. Return concrete types.
- No package-level state outside `cmd/speki`. Loggers, clients and clocks are passed in. Two exceptions:
  Prometheus metrics are package-level `promauto` variables, so every metric is registered without
  anyone wiring it up, and `store` sets go-redis's logger, which go-redis keeps globally.

## Errors

One action per statement. Check its error on the next line and return. Never pass a call's results
straight into another call that checks them.

```go
// Wrong
doc, err := document.Parse(m.Blob(sha))

// Right
raw, err := m.Blob(sha)
if err != nil {
	return nil, fmt.Errorf("read blob %s: %w", sha, err)
}
doc, err := document.Parse(raw)
if err != nil {
	return nil, err
}
```

- Wrap with `%w` and say what was being done, not that it failed: `fmt.Errorf("fetch %s: %w", v.ID, err)`.
- Log an error or return it, not both.
- Log with the `...Context` methods whenever a context is in scope: the handler takes the request ID from it.
- Errors that callers act on are sentinel values or types (`gh.ErrHeadMoved`), checked with `errors.Is` and
  `errors.As`.

## Declarations

- One `var` per value, not a `var (...)` block, so each reads at top level.
- Arguments on their own lines when there are more than two, one per line with a trailing comma.
- Struct literals with more than three fields go one field per line.

## Concurrency

- Every goroutine is started with a context it watches, by someone who waits for it to end.
- Bounded fan-out uses `errgroup` with `SetLimit`.
- A mutex guards data. Channels hand work between goroutines. Don't mix the two for one thing.

## Security rules

- Code that returns document content takes an `*access.Grant`. No exceptions without a comment explaining
  why.
- Tokens, codes, cookies, the configuration (it holds secrets), document content and SQL never go into
  logs or error messages. The slog handler redacts known attribute names, but don't rely on it.
- URLs that end up in tokens or metadata come from configuration, never from request headers.

## Tests

Tests need `-tags 'sqlite_fts5,sqlite_omit_load_extension'`, the same tags as the build. `task test` sets them.

### Files and packages

- Test files end in `_test.go` and use a `_test` package. Test only through the exported API.
- Code that must reach unexported names goes in `<name>_internal_test.go`, keeps the package name, and
  has a one-line comment saying why.
- To expose one internal to black-box tests, use `export_test.go`, not a public API.

### Fakes, mocks, helpers

- Fakes go in `<name>_fake.go` next to the real code. An exported `NewFake(tb testing.TB, opts...)`
  returns a started, ready instance, registers teardown with `tb.Cleanup`, and fails setup with
  `tb.Fatal`.
- A fake embeds the real type and swaps only the edges (I/O, remote calls). It doesn't reimplement the
  logic.
- Mocks are generated with `mockgen` into a `mock/` subpackage. The `//go:generate` line goes in the
  test file that uses them.
- Helpers another package needs go in a `test/` subfolder next to the code. Helpers for a whole library
  go in a sibling `<pkg>test` package, like `httptest`.
- Helpers take `testing.TB` and call `tb.Helper()` first.
- Shared expectations are option funcs, `func(testing.TB, *SDK)`, named `Expect<Thing>` and
  `Expect<Thing>_<Case>` (`ExpectUser_NotFound`). Tests combine them into a setup.

### Test bodies

- Table-driven: `testCases := []struct{ name string; ... }`, ranged with `t.Run(testCase.name, ...)`.
- `t.Parallel()` at the top of every test and subtest.
- `t.Context()`, not `context.Background()`.
- Standard library plus `go-cmp`. No assertion libraries. `miniredis` is allowed as a Redis wire fake
  and `prometheus/testutil` for reading metrics.
- `t.Fatalf` when the test can't go on, `t.Errorf` otherwise. Messages read "got X, want Y". Structs
  are compared with `cmp.Diff` and reported as "(-want +got)".
- `httptest` for HTTP, `t.Setenv` for environment variables, `testdata/` for fixtures.
- No `time.Sleep` to sync. Wait on a channel or condition, or inject a clock. `testing/synctest`
  counts as an injected clock.

### Benchmarks

- `Benchmark<Name>` sits beside the test and uses the same fake setup.
- Simulated delays in fakes are off. Loop with `b.Loop()`.

### What to test

- Behavior at the public boundary: the HTTP route, the exported func. Internal helpers are covered
  through it, not tested one by one.
- Everything outside the package's domain is faked at the wire: backend calls, message bus, k8s,
  clocks. Assert on the request sent and the response returned, never on whether the other system works.
- Not third-party or generated code (mocks, specs, protobuf), and not plain types or constants with no
  logic.
- Our own glue to outside systems is tested: mapping, error translation, retries, caching.

### How much

- Services: one route test per route and version. Library code: unit tests on the exported API.
- Every route covers the happy path with the full output compared, each upstream failure the code
  handles differently (not found, deleted, forbidden), and each input it rejects (invalid, empty,
  missing, zero, negative).
- A case is added only when it takes a different code path or gives a different result.
- Shared library code gets more: edge cases, concurrency under `-race`, caching.
- Benchmarks for hot paths and per-route handlers only.
- No coverage target. A missing test file is fine when a higher-level test covers the code.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):
`<type>(<optional scope>): <summary>`, with the summary in lower case, imperative, and no trailing period.

- Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`.
- The scope, when it helps, is the package: `fix(index): skip files that aren't UTF-8`.
- A breaking change gets a `!` after the type or scope and a `BREAKING CHANGE:` footer explaining it.
- The body says why, when the summary doesn't.
