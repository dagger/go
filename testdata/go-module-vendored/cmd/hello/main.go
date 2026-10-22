// Command hello prints a greeting from a vendored dependency.
package main

import (
	"fmt"

	"example.com/greet"
)

func main() { fmt.Println(greet.Hello("vendor")) }
