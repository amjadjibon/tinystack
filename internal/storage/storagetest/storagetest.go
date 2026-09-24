// Package storagetest checks that a storage.Store backend keeps the contract
// every service relies on: atomic commits, rollback, serializable updates, and
// values that never alias the caller's bytes.
package storagetest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// Run runs the contract against stores returned by open, which returns an
// empty store for each subtest.
func Run(t *testing.T, open func(t *testing.T) storage.Store) {
	t.Helper()
	t.Run("transactions", func(t *testing.T) { transactions(t, open) })
	t.Run("concurrent updates", func(t *testing.T) { concurrentUpdates(t, open(t)) })
	t.Run("copies", func(t *testing.T) { copies(t, open(t)) })
}

func transactions(t *testing.T, open func(t *testing.T) storage.Store) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		fail   bool
		cancel bool
	}{{"commit", false, false}, {"rollback", true, false}, {"cancel", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := s.Update(ctx, func(tx storage.WriteTx) error {
				tx.Put("a", []byte("new"))
				tx.Put("b", []byte("new"))
				if tc.cancel {
					cancel()
				}

				if tc.fail {
					return errors.New("abort")
				}

				return nil
			})
			if (err != nil) != (tc.fail || tc.cancel) {
				t.Fatalf("commit error: %v", err)
			}

			want := 2
			if tc.fail || tc.cancel {
				want = 0
			}

			if got := len(Snapshot(t, s)); got != want {
				t.Fatalf("committed %d keys, want %d", got, want)
			}
		})
	}
}

func concurrentUpdates(t *testing.T, s storage.Store) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			err := s.Update(ctx, func(tx storage.WriteTx) error {
				v, _ := tx.Get("counter")
				n := 0
				if len(v) > 0 {
					var err error
					n, err = strconv.Atoi(string(v))
					if err != nil {
						return err
					}
				}

				tx.Put("counter", []byte(strconv.Itoa(n+1)))
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	if got := string(Snapshot(t, s)["counter"]); got != "50" {
		t.Fatalf("lost update: counter is %q", got)
	}
}

func copies(t *testing.T, s storage.Store) {
	t.Helper()
	const safe = "safe"
	ctx := context.Background()
	err := s.Update(ctx, func(tx storage.WriteTx) error {
		v := []byte(safe)
		tx.Put("copy", v)
		tx.Put("gone", v)
		v[0] = 'X'
		tx.Delete("gone")
		if _, ok := tx.Get("gone"); ok {
			t.Error("deleted value visible")
		}

		if got, _ := tx.Get("copy"); string(got) != safe {
			t.Errorf("staged write aliases caller bytes: %q", got)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.View(ctx, func(tx storage.ReadTx) error {
		v, ok := tx.Get("copy")
		if !ok || string(v) != safe {
			t.Fatalf("write aliases caller bytes: %q", v)
		}

		v[0] = 'X'
		if again, _ := tx.Get("copy"); string(again) != safe {
			t.Fatal("read aliases stored bytes")
		}

		if _, ok = tx.Get("gone"); ok {
			t.Fatal("delete did not commit")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Snapshot returns every committed record in s.
func Snapshot(t *testing.T, s storage.Store) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := s.View(context.Background(), func(tx storage.ReadTx) error {
		for _, k := range tx.Keys() {
			out[k], _ = tx.Get(k)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}
