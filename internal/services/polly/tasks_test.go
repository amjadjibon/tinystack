package polly

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestRecoverClaimedTask(t *testing.T) {
	t.Parallel()
	store := &memory.Store{}
	s := New(store, nil)
	job := work{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}, ID: "claimed"}
	ctx := request.WithMetadata(t.Context(), request.Metadata{Scope: job.Scope})
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		task := api.SynthesisTask{TaskId: new(job.ID), TaskStatus: new(inProgress)}
		if err := storage.Write(tx, job.taskKey(), task); err != nil {
			return err
		}

		return storage.Write(tx, job.indexKey(), job)
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	if startErr := s.Start(ctx); startErr != nil {
		t.Fatal(startErr)
	}

	out, err := s.GetSpeechSynthesisTask(ctx, &api.GetSpeechSynthesisTaskInput{TaskId: new(job.ID)})
	if err != nil {
		t.Fatal(err)
	}

	if task := out.SynthesisTask; common.Value(task.TaskStatus) != failed ||
		common.Value(task.TaskStatusReason) != "interrupted by service restart" {
		t.Fatalf("recovered task = %+v", task)
	}

	err = store.View(ctx, func(tx storage.ReadTx) error {
		if _, ok := tx.Get(job.indexKey()); ok {
			t.Error("recovered work remains queued")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
