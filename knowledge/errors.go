package knowledge

import "fmt"

// Code says what kind of failure an Error is. Agents see it first, and act on it.
type Code string

// The codes the read tools return.
const (
	NotFound Code = "not_found"
	Invalid  Code = "invalid"
)

// Error is a failure the agent can do something about. Its message says what happened and what to do
// next. Any other error is speki's or GitHub's problem.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
