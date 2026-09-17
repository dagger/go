// Package cgo requires a C++ toolchain during analysis.
package cgo

/*
int answer(void);
*/
import "C"

func Answer() int {
	return int(C.answer())
}
