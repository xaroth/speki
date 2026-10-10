package knowledge

import "fmt"

// Code says what kind of failure an Error is. Agents see it first, and act on it.
type Code string

// The codes tools return. docs/functional/tools.md says what each means.
const (
	NotFound        Code = "not_found"
	Conflict        Code = "conflict"
	AlreadyExists   Code = "already_exists"
	Invalid         Code = "invalid"
	NoWriteAccess   Code = "no_write_access"
	ReadOnlySession Code = "read_only_session"
	VaultReadOnly   Code = "vault_read_only"
	SecretDetected  Code = "secret_detected"
	TooLarge        Code = "too_large"
	Timeout         Code = "timeout"
	RateLimited     Code = "rate_limited"
	Unavailable     Code = "unavailable"
)

// Error is a failure the agent can do something about. Its message says what happened and what to do
// next. Any other error is speki's or GitHub's problem.
type Error struct {
	Code    Code
	Message string
	Version string // the document's current version, for conflict and already_exists
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
