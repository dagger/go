package gomodulewithtestdata

// workspace:include ../*.data
// workspace:include /LICENSE

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

func TestWorkspaceIncludeAnnotation(t *testing.T) {
	data, err := os.ReadFile("../workspace-include.data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "mounted from workspace include" {
		t.Fatalf("unexpected workspace include contents: %q", data)
	}

	license, err := os.ReadFile("../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(license), "Apache License") {
		t.Fatalf("unexpected license contents: %q", license)
	}
}
