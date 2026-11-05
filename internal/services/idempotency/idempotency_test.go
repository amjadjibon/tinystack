package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestReplayWindowAndFingerprint(t *testing.T) {
	store := &memory.Store{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scope := request.Scope{AccountID: "test", Region: "us-east-1"}
	key := Key(scope, "test", "Create", new("token"))
	input := struct {
		ID string `json:"-"`
	}{ID: "first"}
	mismatch := errors.New("parameters differ")
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		if err := Remember(tx, key, input, "original", now.Add(time.Minute)); err != nil {
			return err
		}
		result, found, err := Replay[string](tx, key, input, mismatch, now)
		if err != nil || !found || result != "original" {
			t.Fatalf("replay: %q %v %v", result, found, err)
		}
		changed := input
		changed.ID = "second"
		_, found, err = Replay[string](tx, key, changed, mismatch, now)
		if !errors.Is(err, mismatch) || found {
			t.Fatalf("mismatch: %v %v", found, err)
		}
		_, found, err = Replay[string](tx, key, changed, mismatch, now.Add(time.Minute))
		if err != nil || found {
			t.Fatalf("expired token: %v %v", found, err)
		}
		return Remember(tx, key, changed, "replacement")
	})
	if err != nil {
		t.Fatal(err)
	}
	err = store.View(context.Background(), func(tx storage.ReadTx) error {
		input.ID = "second"
		result, found, err := Replay[string](tx, key, input, mismatch, now.Add(24*time.Hour))
		if err != nil || !found || result != "replacement" {
			t.Fatalf("nonexpiring replacement: %q %v %v", result, found, err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
