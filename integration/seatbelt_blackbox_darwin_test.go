//go:build integration && darwin

package integration_test

import (
	"os"
	"testing"
)

func TestRuntimeBlackBoxSeatbelt(t *testing.T) {
	if os.Getenv("PONS_SEATBELT_TEST") != "1" {
		t.Skip("set PONS_SEATBELT_TEST=1 to run the compiled pons-hands/Seatbelt black-box test")
	}
	exerciseRuntime(t, true)
}
