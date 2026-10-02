package fixture

import (
	"io"
	"net/http"
	"os"
	"testing"
)

//go:test:container dag://go-dev/test-env
//go:test:dependency directive-backend dag+service://go-dev/dependency-service
//go:test:include input.txt
func TestSelectedContainer(t *testing.T) {
	if os.Getenv("TESTER_BASE") != "workspace" {
		t.Fatal("test did not use the selected workspace container")
	}
	if os.Getenv("GOCACHE") != "/root/.cache/go-build" || os.Getenv("GOMODCACHE") != "/go/pkg/mod" {
		t.Fatal("selected container lost Go caches")
	}
	config, err := os.ReadFile("/tester-config")
	if err != nil || string(config) != "mounted\n" {
		t.Fatalf("selected container lost mounted file: %q, %v", config, err)
	}
	input, err := os.ReadFile("input.txt")
	if err != nil || string(input) != "included test input\n" {
		t.Fatalf("test include was not mounted: %q, %v", input, err)
	}
	response, err := http.Get("http://tester-input:8080/value")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "test service retained\n" {
		t.Fatalf("selected container lost service: %q, %v", body, err)
	}
	response, err = http.Get("http://directive-backend:8080/value")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(response.Body)
	if err != nil || string(body) != "dependency service\n" {
		t.Fatalf("named test lost dependency: %q, %v", body, err)
	}
}
