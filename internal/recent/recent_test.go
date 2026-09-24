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
