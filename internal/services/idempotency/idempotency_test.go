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
	scope := request.Scope{AccountID: "test", Region: "us-east-1"}
	live := Key(scope, "test", "Create", new("live"))
	expired := Key(scope, "test", "Create", new("expired"))
	input := struct {
		ID string `json:"-"`
	}{ID: "first"}
	changed := input
	changed.ID = "second"
	mismatch := errors.New("parameters differ")

	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		if err := Remember(tx, live, input, "original", time.Now().Add(time.Hour)); err != nil {
			return err
		}

		return Remember(tx, expired, input, "stale", time.Now().Add(-time.Second))
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.View(context.Background(), func(tx storage.ReadTx) error {
		result, found, err := Replay[string](tx, live, input, mismatch)
		if err != nil || !found || result != "original" {
			t.Fatalf("replay: %q %v %v", result, found, err)
		}

		_, found, err = Replay[string](tx, live, changed, mismatch)
		if !errors.Is(err, mismatch) || found {
			t.Fatalf("mismatch on json:\"-\" field: %v %v", found, err)
		}

		_, found, err = Replay[string](tx, expired, changed, mismatch)
		if err != nil || found {
			t.Fatalf("expired token: %v %v", found, err)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
