package gomodulewithtestdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
)

func TestDaggerSessionAvailableFromGoTest(t *testing.T) {
	// Tests run with a session of their own rather than the one running them,
	// so that a module address resolves against the user's workspace rather
	// than against the toolchain module.
	port := os.Getenv("DAGGER_SESSION_PORT")
	token := os.Getenv("DAGGER_SESSION_TOKEN")
	if port == "" || token == "" {
		t.Fatal("DAGGER_SESSION_PORT and DAGGER_SESSION_TOKEN must be set")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(`{
			host {
				directory(path: %q) { entries }
			}
			currentWorkspace {
				id
				directory(path: "/", include: ["**/go.mod"]) { entries }
			}
		}`, wd),
	})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+"/query", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.SetBasicAuth(token, "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected Dagger response status %s: %s", resp.Status, body)
	}

	var result struct {
		Data struct {
			Host struct {
				Directory struct {
					Entries []string `json:"entries"`
				} `json:"directory"`
			} `json:"host"`
			CurrentWorkspace struct {
				ID        string `json:"id"`
				Directory struct {
					Entries []string `json:"entries"`
				} `json:"directory"`
			} `json:"currentWorkspace"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode Dagger response: %v: %s", err, body)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("Dagger query returned errors: %s", body)
	}

	entries := result.Data.Host.Directory.Entries
	if !contains(entries, "go.mod") || !contains(entries, "testdata/") {
		t.Fatalf("unexpected nested Dagger host directory entries: %v", entries)
	}

	if result.Data.CurrentWorkspace.ID == "" {
		t.Fatal("nested Dagger workspace did not return an ID")
	}
	// The workspace a test sees is the one its container carries, so it holds
	// what the runner mounted -- the Go sources -- not every file of the
	// repository the tests came from.
	if len(result.Data.CurrentWorkspace.Directory.Entries) == 0 {
		t.Fatal("nested Dagger workspace held no Go module")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
