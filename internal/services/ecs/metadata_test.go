package ecs

import (
	"context"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestContainerMetadataRemovedOnStop(t *testing.T) {
	ctx := context.Background()
	store := &memory.Store{}
	s := New(store)
	defer s.Stop()
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	task := &task{ID: "task", Cluster: "default", Last: running, Metadata: map[string]ContainerMetadata{
		"token": {TaskID: "task", ContainerName: "web"},
	}}
	key := taskKey(scope, task.Cluster, task.ID)
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		if err := storage.Write(tx, key, task); err != nil {
			return err
		}

		return storage.Write(tx, "aws/ecs/metadata/token", key)
	}); err != nil {
		t.Fatal(err)
	}

	if rec, found, err := s.ContainerMetadataByToken(ctx, "token"); err != nil || !found || rec.ContainerName != "web" {
		t.Fatalf("running metadata: %v %v %v", rec, found, err)
	}

	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return s.stop(tx, scope, task, "UserInitiated", "test")
	}); err != nil {
		t.Fatal(err)
	}

	if _, found, err := s.ContainerMetadataByToken(ctx, "token"); err != nil || found {
		t.Fatalf("stopped metadata: found=%v err=%v", found, err)
	}

	if err := store.View(ctx, func(tx storage.ReadTx) error {
		if _, found := tx.Get("aws/ecs/metadata/token"); found {
			t.Fatal("metadata index retained after stop")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := store.Update(
		ctx,
		func(tx storage.WriteTx) error { return s.purge(tx, scope, task.Cluster) },
	); err != nil {
		t.Fatal(err)
	}

	if err := store.View(ctx, func(tx storage.ReadTx) error {
		if _, found := tx.Get(key); found {
			t.Fatal("stopped task retained after purge")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
