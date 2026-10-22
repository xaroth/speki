package knowledge

import (
	"errors"
	"fmt"

	"github.com/xaroth/speki/library/index"
)

// ErrRebuilding is what Vaults.Sync returns when a vault's index needs a full rebuild before it can catch
// up, which isn't done for a waiting request.
var ErrRebuilding = errors.New("the vault's index is being rebuilt")

// Code says what kind of failure an Error is. Agents see it first, and act on it.
type Code string

// The codes tools return. docs/tools/overview.md says what each means.
const (
	CodeNotFound        Code = "not_found"
	CodeConflict        Code = "conflict"
	CodeAlreadyExists   Code = "already_exists"
	CodeInvalid         Code = "invalid"
	CodeBroken          Code = "broken"
	CodeNoWriteAccess   Code = "no_write_access"
	CodeReadOnlySession Code = "read_only_session"
	CodeVaultReadOnly   Code = "vault_read_only"
	CodeSecretDetected  Code = "secret_detected"
	CodeTooLarge        Code = "too_large"
	CodeTimeout         Code = "timeout"
	CodeRateLimited     Code = "rate_limited"
	CodeUnavailable     Code = "unavailable"
)

// Error is a failure the agent can do something about. Its message says what happened and what to do
// next. Any other error is speki's or GitHub's problem.
type Error struct {
	Code    Code
	Message string
	Version string // the document's current version, for conflict, already_exists and broken
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// signedOut is the error for a user whose GitHub token GitHub no longer accepts.
func signedOut() *Error {
	return errorf(CodeUnavailable, "GitHub no longer accepts the user's sign-in. Ask them to reconnect speki")
}

// brokenDocument is the error for reading or changing a document whose file speki can't parse. Delete
// is the only change it allows: nobody can have read the current content through speki.
func brokenDocument(d index.Document) *Error {
	return &Error{
		Code:    CodeBroken,
		Message: fmt.Sprintf("%s can't be parsed (%s). Ask the user to fix it in git. Speki only allows deleting it, with version %s", d.Path, d.Broken, d.Version),
		Version: d.Version,
	}
}

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
