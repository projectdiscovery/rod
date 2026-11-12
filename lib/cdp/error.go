package cdp

import (
	"fmt"
)

// Error of the Response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// Error stdlib interface.
func (e *Error) Error() string {
	return fmt.Sprintf("%v", *e)
}

// errTargetNavigated is the message Chrome 157 uses where older versions say
// "Execution context was destroyed.": a pending evaluation was cut off by a
// navigation or close of the inspected target.
const errTargetNavigated = "Inspected target navigated or closed"

// Is stdlib interface. ErrCtxDestroyed also matches the navigation message,
// so the retry logic keeps working on newer Chrome.
func (e Error) Is(target error) bool {
	err, ok := target.(*Error)
	if !ok {
		return false
	}
	if e == *err {
		return true
	}
	return err == ErrCtxDestroyed && e.Code == ErrCtxDestroyed.Code && e.Message == errTargetNavigated
}

// ErrCtxNotFound type.
var ErrCtxNotFound = &Error{
	Code:    -32000,
	Message: "Cannot find context with specified id",
}

// ErrSessionNotFound type.
var ErrSessionNotFound = &Error{
	Code:    -32001,
	Message: "Session with given id not found.",
}

// ErrSearchSessionNotFound type.
var ErrSearchSessionNotFound = &Error{
	Code:    -32000,
	Message: "No search session with given id found",
}

// ErrCtxDestroyed type.
var ErrCtxDestroyed = &Error{
	Code:    -32000,
	Message: "Execution context was destroyed.",
}

// ErrObjNotFound type.
var ErrObjNotFound = &Error{
	Code:    -32000,
	Message: "Could not find object with given id",
}

// ErrNodeNotFoundAtPos type.
var ErrNodeNotFoundAtPos = &Error{
	Code:    -32000,
	Message: "No node found at given location",
}

// ErrNotAttachedToActivePage type.
var ErrNotAttachedToActivePage = &Error{
	Code:    -32000,
	Message: "Not attached to an active page",
}
