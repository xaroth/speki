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
- No package-level state outside `cmd/speki`. Loggers, clients and clocks are passed in.

## Errors

One action per statement. Check its error on the next line and return. Never pass a call's results straight
into another call that checks them.

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
- Tokens, codes, cookies, document content and SQL never go into logs or error messages. The slog handler
  redacts known attribute names, but don't rely on it.
- URLs that end up in tokens or metadata come from configuration, never from request headers.

## Tests

- Table-driven with `t.Run`, helpers call `t.Helper()`.
- Hand-written fakes for the interfaces a package declares. No mock generators.
- Fixtures in `testdata/`. Compare with `go-cmp`.
- No `time.Sleep` to wait for something. Use `testing/synctest`, channels, or a fake clock.
- `t.Context()` for contexts.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):
`<type>(<optional scope>): <summary>`, with the summary in lower case, imperative, and no trailing period.

- Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`.
- The scope, when it helps, is the package: `fix(index): skip files that aren't UTF-8`.
- A breaking change gets a `!` after the type or scope and a `BREAKING CHANGE:` footer explaining it.
- The body says why, when the summary doesn't.
