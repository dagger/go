package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
)

func main() {
	endpoint := os.Getenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST")
	if !strings.HasPrefix(endpoint, "tcp://") {
		panic("selected container has no dev engine endpoint: " + endpoint)
	}
	cmd := exec.Command("dagger", "--engine="+endpoint, "api", "query", "-M")
	// Use the selected container's service, not the parent Dagger session.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "DAGGER_SESSION_PORT=") && !strings.HasPrefix(env, "DAGGER_SESSION_TOKEN=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Stdin = bytes.NewBufferString(`{ container { from(address: "alpine:3.23") { withExec(args: ["echo", "dev-engine-connected"]) { stdout } } } }`)
	cmd.Stderr = os.Stderr
	result, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("generated.txt", result, 0644); err != nil {
		panic(err)
	}
}
