package sns

import (
	"context"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestSMSOptOutState(t *testing.T) {
	store := memory.New(nil, nil)
	svc := New(store, nil, nil, nil, delivery.New())
	t.Cleanup(svc.Stop)

	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope, AccessKey: "test"})
	num := "+15555550199"

	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key("sns", kindSMS, smsOptedOutPrefix+num), &optedOutRecord{PhoneNumber: num})
	}); err != nil {
		t.Fatal(err)
	}

	checked, err := svc.CheckIfPhoneNumberIsOptedOut(ctx, &api.CheckIfPhoneNumberIsOptedOutInput{PhoneNumber: new(num)})
	if err != nil || !common.Value(checked.IsOptedOut) {
		t.Fatalf("CheckIfPhoneNumberIsOptedOut = %+v, %v", checked, err)
	}

	listed, err := svc.ListPhoneNumbersOptedOut(ctx, &api.ListPhoneNumbersOptedOutInput{})
	if err != nil || len(listed.PhoneNumbers) != 1 || listed.PhoneNumbers[0] != num {
		t.Fatalf("ListPhoneNumbersOptedOut = %+v, %v", listed, err)
	}

	_, err = svc.CreateSMSSandboxPhoneNumber(ctx, &api.CreateSMSSandboxPhoneNumberInput{PhoneNumber: new(num)})
	if e := awserror.From(err); e == nil || e.Code != "OptedOutException" {
		t.Fatalf("CreateSMSSandboxPhoneNumber opted-out err = %v", err)
	}

	if _, err = svc.OptInPhoneNumber(ctx, &api.OptInPhoneNumberInput{PhoneNumber: new(num)}); err != nil {
		t.Fatal(err)
	}

	checked, err = svc.CheckIfPhoneNumberIsOptedOut(ctx, &api.CheckIfPhoneNumberIsOptedOutInput{PhoneNumber: new(num)})
	if err != nil || common.Value(checked.IsOptedOut) {
		t.Fatalf("after OptInPhoneNumber = %+v, %v", checked, err)
	}

	listed, err = svc.ListPhoneNumbersOptedOut(ctx, &api.ListPhoneNumbersOptedOutInput{})
	if err != nil || len(listed.PhoneNumbers) != 0 {
		t.Fatalf("ListPhoneNumbersOptedOut after OptIn = %+v, %v", listed, err)
	}
}
