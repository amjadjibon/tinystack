package common_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestValue(t *testing.T) {
	t.Parallel()

	var nilStr *string
	if got := common.Value(nilStr); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}

	s := "hello"
	if got := common.Value(&s); got != "hello" {
		t.Fatalf("expected hello, got %q", got)
	}

	var nilInt *int
	if got := common.Value(nilInt); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestPtr(t *testing.T) {
	t.Parallel()

	p := common.Ptr("world")
	if p == nil || *p != "world" {
		t.Fatalf("unexpected pointer result: %v", p)
	}
}

func TestOrElse(t *testing.T) {
	t.Parallel()

	fallback := common.Ptr("fallback")
	primary := common.Ptr("primary")

	if got := common.OrElse(nil, fallback); got != fallback {
		t.Fatalf("expected fallback, got %v", got)
	}
	if got := common.OrElse(primary, fallback); got != primary {
		t.Fatalf("expected primary, got %v", got)
	}
}

func TestClone(t *testing.T) {
	t.Parallel()

	if got := common.Clone[sample](nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}

	orig := &sample{Name: "test", Count: 42}
	copied := common.Clone(orig)
	if copied == nil || copied == orig {
		t.Fatalf("clone returned identical pointer or nil: %p, %p", orig, copied)
	}
	if *copied != *orig {
		t.Fatalf("clone values do not match: %+v vs %+v", *copied, *orig)
	}
}

func TestCloneTags(t *testing.T) {
	t.Parallel()

	if got := common.CloneTags(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}

	orig := map[string]string{"env": "test", "app": "tinystack"}
	cloned := common.CloneTags(orig)
	if len(cloned) != len(orig) {
		t.Fatalf("length mismatch: %d vs %d", len(cloned), len(orig))
	}
	cloned["env"] = "prod"
	if orig["env"] != "test" {
		t.Fatal("mutation of clone affected original map")
	}
}

func TestRandomID(t *testing.T) {
	t.Parallel()

	id := common.RandomID(10)
	if len(id) != 10 {
		t.Fatalf("expected length 10, got %d (%q)", len(id), id)
	}
	for _, c := range id {
		if !strings.ContainsRune(common.AlphabetLowerHex, c) {
			t.Fatalf("unexpected character %c in id %q", c, id)
		}
	}

	upperID := common.RandomID(8, common.AlphabetUpperHex)
	if len(upperID) != 8 {
		t.Fatalf("expected length 8, got %d (%q)", len(upperID), upperID)
	}
	for _, c := range upperID {
		if !strings.ContainsRune(common.AlphabetUpperHex, c) {
			t.Fatalf("unexpected character %c in upperID %q", c, upperID)
		}
	}
}

func TestTimeHelpers(t *testing.T) {
	t.Parallel()

	tm := time.UnixMilli(1600000000500)
	sec := common.Seconds(tm)
	if sec == nil || *sec != 1600000000 {
		t.Fatalf("unexpected Seconds: %v", sec)
	}

	secMilli := common.SecondsMilli(tm)
	if secMilli == nil || *secMilli != 1600000000.5 {
		t.Fatalf("unexpected SecondsMilli: %v", secMilli)
	}

	u := common.Unix(tm)
	if u != 1600000000.5 {
		t.Fatalf("unexpected Unix: %v", u)
	}
}

func TestErrorsOr(t *testing.T) {
	t.Parallel()

	primary := errors.New("primary error")
	fallback := errors.New("fallback error")

	if got := common.ErrorsOr(primary, fallback); !errors.Is(got, primary) {
		t.Fatalf("expected primary error, got %v", got)
	}
	if got := common.ErrorsOr(nil, fallback); !errors.Is(got, fallback) {
		t.Fatalf("expected fallback error, got %v", got)
	}
}
