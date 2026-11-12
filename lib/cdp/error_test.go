package cdp

import (
	"errors"
	"testing"
)

func TestErrCtxDestroyedMatchesNavigation(t *testing.T) {
	nav := &Error{Code: -32000, Message: errTargetNavigated}
	if !errors.Is(nav, ErrCtxDestroyed) {
		t.Fatal("the navigation message must match ErrCtxDestroyed")
	}
	if !errors.Is(ErrCtxDestroyed, ErrCtxDestroyed) {
		t.Fatal("ErrCtxDestroyed must match itself")
	}

	other := &Error{Code: -32000, Message: "Some other failure"}
	if errors.Is(other, ErrCtxDestroyed) {
		t.Fatal("an unrelated message must not match ErrCtxDestroyed")
	}
	if errors.Is(&Error{Code: -32001, Message: errTargetNavigated}, ErrCtxDestroyed) {
		t.Fatal("the navigation message with another code must not match")
	}
	if errors.Is(nav, ErrCtxNotFound) {
		t.Fatal("the navigation message must not match other errors")
	}
}
