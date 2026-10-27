// Package fail defines hypergraphia's structured error envelope and exit-code
// mapping. Every command error has a stable machine code so an agent can branch
// on it; with --json it is printed as {"ok":false,"error":{...}}.
package fail

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Exit codes.
const (
	CodeOK             = 0
	CodeInternal       = 1
	CodeUsage          = 2
	CodeNotFound       = 3 // not found or ambiguous id
	CodeSecret         = 4 // secret scan rejected the input
	CodeNotInitialized = 5
	CodeRequiresHuman  = 6
	CodeLocked         = 75 // lock busy; retry
)

// Stable error kinds.
const (
	KindInternal       = "internal"
	KindUsage          = "usage"
	KindNotFound       = "not_found"
	KindAmbiguousID    = "ambiguous_id"
	KindSecret         = "secret_detected"
	KindBodyTooLong    = "body_too_long"
	KindInvalidKind    = "invalid_kind"
	KindInvalidEntry   = "invalid_entry"
	KindNotInitialized = "not_initialized"
	KindRequiresHuman  = "requires_human"
	KindLocked         = "locked"
)

// Error is a structured hypergraphia error. Extra carries kind-specific fields
// (findings, candidates, limit, allowed) merged into the JSON error object.
type Error struct {
	Kind    string
	Message string
	Hint    string
	Extra   map[string]any
	code    int
}

func (e *Error) Error() string { return e.Message }

// Code returns the process exit code for this error.
func (e *Error) Code() int { return e.code }

// New builds an Error.
func New(code int, kind, format string, a ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...), code: code}
}

// With returns e with an extra JSON field set.
func (e *Error) With(key string, v any) *Error {
	if e.Extra == nil {
		e.Extra = map[string]any{}
	}
	e.Extra[key] = v
	return e
}

// WithHint returns e with a hint set.
func (e *Error) WithHint(h string) *Error {
	e.Hint = h
	return e
}

// Usage is a convenience for exit code 2.
func Usage(format string, a ...any) *Error {
	return New(CodeUsage, KindUsage, format, a...)
}

// NotFound is a convenience for exit code 3.
func NotFound(format string, a ...any) *Error {
	return New(CodeNotFound, KindNotFound, format, a...)
}

// NotInitialized reports a repo without hypergraphia state.
func NotInitialized() *Error {
	return New(CodeNotInitialized, KindNotInitialized, "hypergraphia is not initialized in this repository").
		WithHint("run `hypergraphia init`")
}

// RequiresHuman reports a command an agent may not run.
func RequiresHuman(what string) *Error {
	return New(CodeRequiresHuman, KindRequiresHuman, "%s must be run by a human at a terminal", what)
}

// Classify maps an arbitrary error to an Error. *Error passes through;
// everything else is internal.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: KindInternal, Message: err.Error(), code: CodeInternal}
}

// JSON renders the error envelope.
func (e *Error) JSON() string {
	obj := map[string]any{"code": e.Kind, "message": e.Message}
	if e.Hint != "" {
		obj["hint"] = e.Hint
	}
	for k, v := range e.Extra {
		obj[k] = v
	}
	b, _ := json.Marshal(map[string]any{"ok": false, "error": obj})
	return string(b)
}
