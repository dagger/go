package main

import (
	"fmt"
	"os"
)

func main() {
	if got := os.Getenv("DAGGER_GO_CUSTOM_BASE"); got != "yes" {
		panic(fmt.Sprintf("DAGGER_GO_CUSTOM_BASE = %q, want yes", got))
	}
	if _, err := os.Stat("/custom-go-base"); err != nil {
		panic(err)
	}

	const contents = `package custombase

const GeneratedFromBase = true
`
	if err := os.WriteFile("generated.go", []byte(contents), 0o644); err != nil {
		panic(err)
	}
}
