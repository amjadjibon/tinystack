package resourceexplorer2_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedValidation(t *testing.T) {
	s := resourceexplorer2.New(memory.New(nil, nil))
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
	for _, tt := range []struct {
		input any
		name  string
	}{
		{name: "CreateView", input: &api.CreateViewInput{}},
		{name: "CreateView", input: &api.CreateViewInput{ViewName: new("bad name")}},
		{name: "CreateView", input: &api.CreateViewInput{ViewName: new(strings.Repeat("x", 65))}},
		{name: "UpdateIndexType", input: &api.UpdateIndexTypeInput{
			Arn:  new("arn:aws:resource-explorer-2:us-east-1:123456789012:index/a"),
			Type: new("OTHER"),
		}},
		{name: "Search", input: &api.SearchInput{}},
		{name: "Search", input: &api.SearchInput{QueryString: new(""), MaxResults: new(int64(0))}},
		{name: "ListIndexes", input: &api.ListIndexesInput{Type: new("OTHER")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := api.Descriptor(s).Operations[tt.name].Invoke(ctx, tt.input)
			var e *awserror.Error
			if !errors.As(err, &e) || e.Code != "ValidationException" {
				t.Fatalf("want ValidationException, got %v", err)
			}
		})
	}
}

func TestCreateIdempotency(t *testing.T) {
	s := resourceexplorer2.New(memory.New(nil, nil))
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
	in := &api.CreateIndexInput{ClientToken: new("index-token")}
	first, err := s.CreateIndex(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	again, err := s.CreateIndex(ctx, in)
	if err != nil || *first.Arn != *again.Arn {
		t.Fatalf("index retry: %+v %v", again, err)
	}

	v := &api.CreateViewInput{ClientToken: new("view-token"), ViewName: new("retry")}
	view, err := s.CreateView(ctx, v)
	if err != nil {
		t.Fatal(err)
	}

	repeat, err := s.CreateView(ctx, v)
	if err != nil || *view.View.ViewArn != *repeat.View.ViewArn {
		t.Fatalf("view retry: %+v %v", repeat, err)
	}
}
