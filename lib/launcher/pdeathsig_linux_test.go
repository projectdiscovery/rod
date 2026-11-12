//go:build linux

package launcher

import "testing"

func TestSetPdeathsigNil(t *testing.T) {
	// A nil attr is ignored instead of panicking.
	setPdeathsig(nil)
}
