package memory_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestTransactionContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   bool
		cancel bool
	}{{"commit", false, false}, {"rollback", true, false}, {"cancel", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &memory.Store{}
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
			err = s.View(context.Background(), func(tx storage.ReadTx) error {
				if len(tx.Keys()) != map[bool]int{true: 0, false: 2}[tc.fail || tc.cancel] {
					t.Fatal("partial or incorrect commit")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentUpdatesAndCopies(t *testing.T) {
	s := &memory.Store{}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if guardErr := s.Update(ctx, func(tx storage.WriteTx) error {
				v, _ := tx.Get("counter")
				n := 0
				if len(v) > 0 {
					var guardErr error
					n, guardErr = strconv.Atoi(string(v))
					if guardErr != nil {
						return guardErr
					}
				}
				tx.Put("counter", []byte(strconv.Itoa(n+1)))
				return nil
			}); guardErr != nil {
				t.Error(guardErr)
			}
		}()
	}
	wg.Wait()
	if guardErr := s.View(ctx, func(tx storage.ReadTx) error {
		v, ok := tx.Get("counter")
		if !ok || string(v) != "50" {
			t.Fatalf("lost update: %s", v)
		}
		v[0] = '0'
		again, _ := tx.Get("counter")
		if string(again) != "50" {
			t.Fatal("read aliases stored bytes")
		}
		return nil
	}); guardErr != nil {
		t.Fatal(guardErr)
	}
	if guardErr := s.Update(ctx, func(tx storage.WriteTx) error {
		v := []byte("safe")
		tx.Put("copy", v)
		v[0] = 'X'
		tx.Delete("counter")
		if _, ok := tx.Get("counter"); ok {
			t.Fatal("deleted value visible")
		}
		return nil
	}); guardErr != nil {
		t.Fatal(guardErr)
	}
	if guardErr := s.View(ctx, func(tx storage.ReadTx) error {
		v, _ := tx.Get("copy")
		if string(v) != "safe" {
			t.Fatal("write aliases caller bytes")
		}
		if _, ok := tx.Get("counter"); ok {
			t.Fatal("delete did not commit")
		}
		return nil
	}); guardErr != nil {
		t.Fatal(guardErr)
	}
}
