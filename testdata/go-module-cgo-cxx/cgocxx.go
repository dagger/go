package cgocxx

// int answer();
import "C"

func Answer() int {
	return int(C.answer())
}
