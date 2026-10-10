package appconfig

import (
	"context"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	data "github.com/amjadjibon/tinystack/internal/aws/generated/appconfigdata"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestExpiredConfigurationToken(t *testing.T) {
	store := &memory.Store{}
	s := New(store).Data()
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(
			tx,
			scope.Key(serviceName, "token", "expired"),
			&session{Expires: time.Now().Add(-time.Second)},
		)
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.GetLatestConfiguration(ctx, &data.GetLatestConfigurationRequest{ConfigurationToken: new("expired")})
	if err == nil || awserror.From(err).Code != "BadRequestException" {
		t.Fatalf("expired token: %v", err)
	}
}
