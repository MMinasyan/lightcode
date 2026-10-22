// Package testassert provides the assertion primitives shared by the Go test
// suites: fatal and nonfatal boolean checks, fatal error presence, and fatal
// comparable equality. It exists so test files share one leaf assertion
// construction instead of repeating the same guard.
package testassert

import "testing"

// Require marks the test failed and stops it when ok is false.
func Require(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

// Check marks the test failed when ok is false and continues execution.
func Check(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

// NoError marks the test failed and stops it when err is non-nil.
func NoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// Equal marks the test failed and stops it when got and want differ.
func Equal[T comparable](t testing.TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
