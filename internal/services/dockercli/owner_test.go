package dockercli

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestRuntimeOwnerPersistsAndIsolatesStores(t *testing.T) {
	store := memory.New(nil, nil)
	owner, err := RuntimeOwner(context.Background(), store, "aws/test/runtime-owner")
	if err != nil || owner == "" {
		t.Fatal(owner, err)
	}

	recovered, err := RuntimeOwner(context.Background(), store, "aws/test/runtime-owner")
	if err != nil || recovered != owner {
		t.Fatal("owner changed on recovery", recovered, err)
	}

	foreign, err := RuntimeOwner(context.Background(), memory.New(nil, nil), "aws/test/runtime-owner")
	if err != nil || foreign == owner || foreign == "" {
		t.Fatal("stores share an owner", foreign, err)
	}
}

func TestRuntimeOwnerCanceledContext(t *testing.T) {
	store := memory.New(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RuntimeOwner(ctx, store, "aws/test/runtime-owner")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled initialization succeeded", err)
	}
}
