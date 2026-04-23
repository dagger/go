package gomodulewithtestdata

import (
	"os"
	"strings"
	"testing"
)

func TestFixtureFromTestdata(t *testing.T) {
	data, err := os.ReadFile("testdata/fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "mounted from testdata" {
		t.Fatalf("unexpected fixture contents: %q", data)
	}
}
