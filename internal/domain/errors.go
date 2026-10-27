// Package domain holds Zirnis's business rules: the double-entry ledger, accounts, payments,
// payouts, FX, cards, disputes, risk, reconciliation and the T24-style core banking records.
// It has no knowledge of HTTP or SQL beyond struct tags.
package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifies a business error; the API maps it to an HTTP status.
type ErrorKind int

const (
	// KindRule is a violated business rule (HTTP 422).
	KindRule ErrorKind = iota
	// KindNotFound is a missing resource (HTTP 404).
	KindNotFound
	// KindConflict is a clash with current state, e.g. a duplicate (HTTP 409).
	KindConflict
)

// Error is a business error with a stable, machine-readable Code that API clients branch on.
type Error struct {
	Kind      ErrorKind
	Code      string
	Message   string
	Overrides []Override
}

func (e *Error) Error() string { return e.Message }

// Rule reports a violated business rule.
func Rule(code, format string, args ...any) *Error {
	return &Error{Kind: KindRule, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Conflict reports a request that conflicts with the current state.
func Conflict(code, format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: fmt.Sprintf(format, args...)}
}

// NotFound reports a missing resource, e.g. code "wallet_not_found".
func NotFound(resource string, id any) *Error {
	return &Error{Kind: KindNotFound, Code: resource + "_not_found", Message: fmt.Sprintf("%s '%v' was not found.", resource, id)}
}

// OverridesRequired is raised when a transaction has T24-style overrides the caller has not accepted.
func OverridesRequired(overrides []Override) *Error {
	codes := ""
	for i, o := range overrides {
		if i > 0 {
			codes += ", "
		}
		codes += o.Code
	}
	return &Error{
		Kind:      KindConflict,
		Code:      "overrides_required",
		Message:   "This transaction raised overrides that must be accepted: " + codes,
		Overrides: overrides,
	}
}

// CodeOf returns the business error code of err, or "" if it is not a domain error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// AsError unwraps err into a domain error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
