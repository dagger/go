package custombase

import (
	"os"
	"testing"
)

func TestBase(t *testing.T) {
	if got := os.Getenv("DAGGER_GO_CUSTOM_BASE"); got != "yes" {
		t.Fatalf("DAGGER_GO_CUSTOM_BASE = %q, want yes", got)
	}
	if _, err := os.Stat("/custom-go-base"); err != nil {
		t.Fatalf("custom base marker file is missing: %v", err)
	}
}
