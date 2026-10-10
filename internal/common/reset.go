package common

// Resettable is a runtime that can drop everything it started, such as
// containers, and keep serving, unlike Close.
type Resettable interface{ Reset() }

// ResetRuntime resets runtime if it can be reset; a runtime without state to
// drop, such as the in-process Lambda runtime, needs nothing.
func ResetRuntime(runtime any) {
	if r, ok := runtime.(Resettable); ok {
		r.Reset()
	}
}
