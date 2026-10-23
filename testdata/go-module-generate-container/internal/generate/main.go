package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	if os.Getenv("GENERATOR_BASE") != "workspace" {
		panic("generator did not use the workspace container")
	}
	config, err := os.ReadFile("/generator-config")
	if err != nil || string(config) != "mounted\n" {
		panic(fmt.Sprintf("generator lost its mounted file: %q, %v", config, err))
	}
	response, err := http.Get("http://generator-input:8080/value")
	if err != nil {
		panic(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		panic("generator service returned " + response.Status)
	}
	output, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("generated.txt", output, 0644); err != nil {
		panic(err)
	}
}
