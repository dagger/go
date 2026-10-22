package vendored

import "testing"

func TestGreeting(t *testing.T) {
	if got := Greeting(); got != "hello vendor" {
		t.Fatalf("Greeting() = %q", got)
	}
}
