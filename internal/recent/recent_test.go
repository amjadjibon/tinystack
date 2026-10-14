package recent

import (
	"slices"
	"strings"
	"testing"
)

func TestLog(t *testing.T) {
	l := New[int](3, "n")
	if got := l.List(); len(got) != 0 {
		t.Fatalf("empty log lists %v", got)
	}

	for i := 1; i <= 5; i++ {
		l.Add(i)
	}

	if got := l.List(); !slices.Equal(got, []int{5, 4, 3}) {
		t.Fatalf("got %v", got)
	}

	l.Clear()
	l.Add(6)
	if got := l.List(); !slices.Equal(got, []int{6}) {
		t.Fatalf("after clear got %v", got)
	}
}

func TestText(t *testing.T) {
	if got := Text([]byte("héllo")); got != "héllo" {
		t.Fatalf("got %q", got)
	}

	// A cut inside a multi-byte rune is replaced, never left invalid.
	long := []byte(strings.Repeat("a", MaxText-1) + "é")
	if got := Text(long); len(got) != MaxText+2 || !strings.HasSuffix(got, "�") {
		t.Fatalf("got %d bytes ending %q", len(got), got[len(got)-3:])
	}
}

func TestRetain(t *testing.T) {
	l := New[int](5, "n")
	for i := 1; i <= 5; i++ {
		l.Add(i)
	}

	l.Retain(func(n int) bool {
		return n%2 != 0
	})
	if got := l.List(); !slices.Equal(got, []int{5, 3, 1}) {
		t.Fatalf("after retain got %v, want [5 3 1]", got)
	}

	l.Add(7)
	l.Add(9)
	l.Add(11)
	if got := l.List(); !slices.Equal(got, []int{11, 9, 7, 5, 3}) {
		t.Fatalf("after wrapping got %v, want [11 9 7 5 3]", got)
	}
}
