package account_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/account"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/account"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func contextWithAccount(accountID string) context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{
			AccountID: accountID,
			Region:    "us-east-1",
		},
	})
}

func ptr[T any](v T) *T { return &v }

func TestExplicitEmptyAccountIDIsInvalid(t *testing.T) {
	svc := account.New(&memory.Store{})
	_, err := svc.GetAccountInformation(contextWithAccount("123456789012"), &api.GetAccountInformationRequest{
		AccountId: ptr(""),
	})
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("explicit malformed AccountId should fail validation, got %v", err)
	}
}

func TestGetAccountInformation(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := contextWithAccount("123456789012")

	// 1. Default account info
	resp, err := svc.GetAccountInformation(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccountId == nil || *resp.AccountId != "123456789012" {
		t.Fatalf("expected AccountId 123456789012, got %v", resp.AccountId)
	}
	if resp.AccountName == nil || *resp.AccountName != "TinyStack Account" {
		t.Fatalf("expected AccountName TinyStack Account, got %v", resp.AccountName)
	}
	if resp.AccountState == nil || *resp.AccountState != "ACTIVE" {
		t.Fatalf("expected AccountState ACTIVE, got %v", resp.AccountState)
	}
	if resp.AccountCreatedDate == nil || resp.AccountCreatedDate.IsZero() {
		t.Fatalf("expected non-zero AccountCreatedDate")
	}

	// 2. Explicit matching AccountId
	resp2, err := svc.GetAccountInformation(ctx, &api.GetAccountInformationRequest{
		AccountId: ptr("123456789012"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *resp2.AccountId != "123456789012" {
		t.Fatalf("expected 123456789012, got %v", *resp2.AccountId)
	}

	// 3. Foreign AccountId rejected with AccessDeniedException
	_, err = svc.GetAccountInformation(ctx, &api.GetAccountInformationRequest{
		AccountId: ptr("999999999999"),
	})
	if err == nil {
		t.Fatalf("expected error for foreign AccountId")
	}
	awsErr := awserror.From(err)
	if awsErr.Code != "AccessDeniedException" {
		t.Fatalf("expected AccessDeniedException, got %v", awsErr.Code)
	}

	// 4. Malformed AccountId rejected with ValidationException
	_, err = svc.GetAccountInformation(ctx, &api.GetAccountInformationRequest{
		AccountId: ptr("not-an-account"),
	})
	if err == nil {
		t.Fatalf("expected error for malformed AccountId")
	}
	awsErr = awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	// 5. Seeded account info
	customDate := time.Date(2023, 6, 15, 12, 0, 0, 0, time.UTC)
	if seedErr := svc.SeedAccount(ctx, "123456789012", "Custom Name", customDate, "SUSPENDED"); seedErr != nil {
		t.Fatalf("seed account error: %v", seedErr)
	}

	resp3, err := svc.GetAccountInformation(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *resp3.AccountName != "Custom Name" {
		t.Fatalf("expected Custom Name, got %v", *resp3.AccountName)
	}
	if !resp3.AccountCreatedDate.Equal(customDate) {
		t.Fatalf("expected %v, got %v", customDate, *resp3.AccountCreatedDate)
	}
	if *resp3.AccountState != "SUSPENDED" {
		t.Fatalf("expected SUSPENDED, got %v", *resp3.AccountState)
	}
}

func TestContactInformation(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := contextWithAccount("123456789012")

	// 1. Missing contact returns ResourceNotFoundException
	_, err := svc.GetContactInformation(ctx, nil)
	if err == nil {
		t.Fatalf("expected error for missing contact information")
	}
	awsErr := awserror.From(err)
	if awsErr.Code != "ResourceNotFoundException" {
		t.Fatalf("expected ResourceNotFoundException, got %v", awsErr.Code)
	}

	// 2. Seed contact information
	contact := api.ContactInformation{
		FullName:      ptr("Alice Smith"),
		AddressLine1:  ptr("100 Main St"),
		City:          ptr("Seattle"),
		StateOrRegion: ptr("WA"),
		PostalCode:    ptr("98101"),
		CountryCode:   ptr("US"),
		PhoneNumber:   ptr("+12065550100"),
		CompanyName:   ptr("Example Corp"),
	}
	if seedErr := svc.SeedContact(ctx, "123456789012", &contact); seedErr != nil {
		t.Fatalf("failed to seed contact: %v", seedErr)
	}

	// 3. Retrieve seeded contact
	resp, err := svc.GetContactInformation(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ContactInformation == nil {
		t.Fatalf("expected non-nil ContactInformation")
	}
	if *resp.ContactInformation.FullName != "Alice Smith" {
		t.Fatalf("expected Alice Smith, got %v", *resp.ContactInformation.FullName)
	}
	if *resp.ContactInformation.City != "Seattle" {
		t.Fatalf("expected Seattle, got %v", *resp.ContactInformation.City)
	}
}

func TestRegionOptStatus(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := contextWithAccount("123456789012")

	// 1. Commercial default region opt-in status (us-east-1 -> ENABLED_BY_DEFAULT)
	resp, err := svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{
		RegionName: ptr("us-east-1"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RegionOptStatus == nil || *resp.RegionOptStatus != "ENABLED_BY_DEFAULT" {
		t.Fatalf("expected ENABLED_BY_DEFAULT for us-east-1, got %v", resp.RegionOptStatus)
	}

	// 2. Opt-in default region status (af-south-1 -> DISABLED)
	respOptIn, err := svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{
		RegionName: ptr("af-south-1"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if respOptIn.RegionOptStatus == nil || *respOptIn.RegionOptStatus != "DISABLED" {
		t.Fatalf("expected DISABLED for af-south-1, got %v", respOptIn.RegionOptStatus)
	}

	// 3. Unknown region returns ValidationException
	_, err = svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{
		RegionName: ptr("invalid-region-99"),
	})
	if err == nil {
		t.Fatalf("expected error for invalid region name")
	}
	awsErr := awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	// 4. Missing/empty region returns ValidationException
	_, err = svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{})
	if err == nil {
		t.Fatalf("expected error for empty region name")
	}
	awsErr = awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	// 5. Seed region opt status
	if seedErr := svc.SeedRegionOptStatus(ctx, "123456789012", "af-south-1", "ENABLED"); seedErr != nil {
		t.Fatalf("failed to seed region opt status: %v", seedErr)
	}
	respUpdated, err := svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{
		RegionName: ptr("af-south-1"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *respUpdated.RegionOptStatus != "ENABLED" {
		t.Fatalf("expected ENABLED, got %v", *respUpdated.RegionOptStatus)
	}
}

func TestListRegions(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := contextWithAccount("123456789012")

	// 1. List all regions
	resp, err := svc.ListRegions(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Regions) != 28 {
		t.Fatalf("expected 28 regions, got %d", len(resp.Regions))
	}

	// 2. Filter by status
	respFilter, err := svc.ListRegions(ctx, &api.ListRegionsRequest{
		RegionOptStatusContains: []string{"DISABLED"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range respFilter.Regions {
		if *r.RegionOptStatus != "DISABLED" {
			t.Fatalf("unexpected region status in filtered results: %s %s", *r.RegionName, *r.RegionOptStatus)
		}
	}

	// 3. Filter with invalid status returns ValidationException
	_, err = svc.ListRegions(ctx, &api.ListRegionsRequest{
		RegionOptStatusContains: []string{"NOT_A_STATUS"},
	})
	if err == nil {
		t.Fatalf("expected error for invalid filter status")
	}
	awsErr := awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	// 4. Pagination with MaxResults
	respPage1, err := svc.ListRegions(ctx, &api.ListRegionsRequest{
		MaxResults: ptr(int64(10)),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respPage1.Regions) != 10 {
		t.Fatalf("expected 10 regions, got %d", len(respPage1.Regions))
	}
	if respPage1.NextToken == nil || *respPage1.NextToken == "" {
		t.Fatalf("expected non-empty NextToken")
	}

	respPage2, err := svc.ListRegions(ctx, &api.ListRegionsRequest{
		MaxResults: ptr(int64(10)),
		NextToken:  respPage1.NextToken,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respPage2.Regions) != 10 {
		t.Fatalf("expected 10 regions on page 2, got %d", len(respPage2.Regions))
	}

	// 5. Invalid MaxResults
	_, err = svc.ListRegions(ctx, &api.ListRegionsRequest{
		MaxResults: ptr(int64(0)),
	})
	if err == nil {
		t.Fatalf("expected error for MaxResults 0")
	}
	awsErr = awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	_, err = svc.ListRegions(ctx, &api.ListRegionsRequest{
		MaxResults: ptr(int64(51)),
	})
	if err == nil {
		t.Fatalf("expected error for MaxResults 51")
	}
	awsErr = awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}

	// 6. Invalid NextToken
	_, err = svc.ListRegions(ctx, &api.ListRegionsRequest{
		NextToken: ptr("!!!bad-token!!!"),
	})
	if err == nil {
		t.Fatalf("expected error for invalid NextToken")
	}
	awsErr = awserror.From(err)
	if awsErr.Code != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", awsErr.Code)
	}
}

func TestSeedFromConfig(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := contextWithAccount("123456789012")

	cfgs := map[string]config.AccountConfig{
		"123456789012": {
			Name:        "Configured Account",
			CreatedDate: "2023-01-15T10:00:00Z",
			State:       "ACTIVE",
			Contact: &config.AccountContactConfig{
				FullName:      "Config Admin",
				AddressLine1:  "100 Main St",
				City:          "Portland",
				StateOrRegion: "OR",
				PostalCode:    "97201",
				CountryCode:   "US",
				PhoneNumber:   "+15035550100",
			},
			Regions: map[string]string{
				"af-south-1": "ENABLED",
			},
		},
	}

	if err := svc.SeedFromConfig(ctx, cfgs); err != nil {
		t.Fatalf("SeedFromConfig failed: %v", err)
	}

	info, err := svc.GetAccountInformation(ctx, nil)
	if err != nil {
		t.Fatalf("GetAccountInformation failed: %v", err)
	}
	if *info.AccountName != "Configured Account" {
		t.Fatalf("expected Configured Account, got %s", *info.AccountName)
	}

	contact, err := svc.GetContactInformation(ctx, nil)
	if err != nil {
		t.Fatalf("GetContactInformation failed: %v", err)
	}
	if *contact.ContactInformation.FullName != "Config Admin" {
		t.Fatalf("expected Config Admin, got %s", *contact.ContactInformation.FullName)
	}

	optStatus, err := svc.GetRegionOptStatus(ctx, &api.GetRegionOptStatusRequest{
		RegionName: ptr("af-south-1"),
	})
	if err != nil {
		t.Fatalf("GetRegionOptStatus failed: %v", err)
	}
	if *optStatus.RegionOptStatus != "ENABLED" {
		t.Fatalf("expected ENABLED, got %s", *optStatus.RegionOptStatus)
	}
}

func TestAccountSQLitePersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store1, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	svc1 := account.New(store1)

	customDate := time.Date(2023, 5, 20, 15, 30, 0, 0, time.UTC)
	if seedErr := svc1.SeedAccount(ctx, "123456789012", "Persisted Name", customDate, "ACTIVE"); seedErr != nil {
		t.Fatal(seedErr)
	}
	if seedErr := svc1.SeedContact(ctx, "123456789012", &api.ContactInformation{
		FullName:      ptr("Persisted Admin"),
		AddressLine1:  ptr("500 Congress Ave"),
		City:          ptr("Austin"),
		StateOrRegion: ptr("TX"),
		PostalCode:    ptr("78701"),
		CountryCode:   ptr("US"),
		PhoneNumber:   ptr("+15125550199"),
	}); seedErr != nil {
		t.Fatal(seedErr)
	}
	if seedErr := svc1.SeedRegionOptStatus(ctx, "123456789012", "af-south-1", "ENABLED"); seedErr != nil {
		t.Fatal(seedErr)
	}

	if closeErr := store1.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	// Reopen the SQLite store
	store2, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	svc2 := account.New(store2)
	ctxAccount := contextWithAccount("123456789012")

	info, err := svc2.GetAccountInformation(ctxAccount, nil)
	if err != nil {
		t.Fatalf("GetAccountInformation failed: %v", err)
	}
	if *info.AccountName != "Persisted Name" {
		t.Fatalf("expected Persisted Name, got %s", *info.AccountName)
	}
	if !info.AccountCreatedDate.Equal(customDate) {
		t.Fatalf("expected %v, got %v", customDate, *info.AccountCreatedDate)
	}

	contact, err := svc2.GetContactInformation(ctxAccount, nil)
	if err != nil {
		t.Fatalf("GetContactInformation failed: %v", err)
	}
	if *contact.ContactInformation.FullName != "Persisted Admin" {
		t.Fatalf("expected Persisted Admin, got %s", *contact.ContactInformation.FullName)
	}
	if *contact.ContactInformation.City != "Austin" {
		t.Fatalf("expected Austin, got %s", *contact.ContactInformation.City)
	}

	optStatus, err := svc2.GetRegionOptStatus(ctxAccount, &api.GetRegionOptStatusRequest{
		RegionName: ptr("af-south-1"),
	})
	if err != nil {
		t.Fatalf("GetRegionOptStatus failed: %v", err)
	}
	if *optStatus.RegionOptStatus != "ENABLED" {
		t.Fatalf("expected ENABLED, got %s", *optStatus.RegionOptStatus)
	}

	// Check another account isolation
	ctxOther := contextWithAccount("999999999999")
	infoOther, err := svc2.GetAccountInformation(ctxOther, nil)
	if err != nil {
		t.Fatalf("GetAccountInformation for other account failed: %v", err)
	}
	if *infoOther.AccountName != "TinyStack Account" {
		t.Fatalf("expected default name for other account, got %s", *infoOther.AccountName)
	}
	_, err = svc2.GetContactInformation(ctxOther, nil)
	if err == nil || awserror.From(err).Code != "ResourceNotFoundException" {
		t.Fatalf("expected ResourceNotFoundException for other account contact, got %v", err)
	}
}

func TestSeedContactValidation(t *testing.T) {
	svc := account.New(&memory.Store{})
	ctx := context.Background()

	validContact := func() *api.ContactInformation {
		return &api.ContactInformation{
			FullName:      ptr("Valid Name"),
			AddressLine1:  ptr("123 Main St"),
			City:          ptr("Seattle"),
			StateOrRegion: ptr("WA"),
			PostalCode:    ptr("98101"),
			CountryCode:   ptr("US"),
			PhoneNumber:   ptr("+12065550100"),
		}
	}

	// 1. Nil contact rejected
	err := svc.SeedContact(ctx, "123456789012", nil)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for nil contact, got %v", err)
	}

	// 2. Invalid AccountId rejected
	err = svc.SeedContact(ctx, "bad-id", validContact())
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for bad AccountId, got %v", err)
	}

	// 3. Incomplete contact (missing AddressLine1) rejected
	incomplete := validContact()
	incomplete.AddressLine1 = nil
	err = svc.SeedContact(ctx, "123456789012", incomplete)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for incomplete contact, got %v", err)
	}

	// 4. Invalid phone number pattern rejected
	badPhone := validContact()
	badPhone.PhoneNumber = ptr("206-555-0100") // missing leading +
	err = svc.SeedContact(ctx, "123456789012", badPhone)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for bad phone format, got %v", err)
	}

	// 5. Missing state for country requiring state
	badState := validContact()
	badState.StateOrRegion = nil
	err = svc.SeedContact(ctx, "123456789012", badState)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for missing state in US, got %v", err)
	}

	// 6. Overlong field rejected
	overlong := validContact()
	overlong.FullName = ptr(strings.Repeat("a", 51))
	err = svc.SeedContact(ctx, "123456789012", overlong)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for overlong FullName, got %v", err)
	}

	// 7. Valid unicode contact succeeds
	unicodeContact := &api.ContactInformation{
		FullName:      ptr("山田 太郎"),
		AddressLine1:  ptr("東京都千代田区千代田1-1"),
		City:          ptr("千代田区"),
		StateOrRegion: ptr("東京都"),
		PostalCode:    ptr("100-8111"),
		CountryCode:   ptr("JP"),
		PhoneNumber:   ptr("+81-3-1234-5678"),
	}
	if seedErr := svc.SeedContact(ctx, "123456789012", unicodeContact); seedErr != nil {
		t.Fatalf("expected successful SeedContact with unicode details, got %v", err)
	}

	// Verify retrieved contact has matching unicode strings
	resp, err := svc.GetContactInformation(contextWithAccount("123456789012"), nil)
	if err != nil {
		t.Fatalf("GetContactInformation failed: %v", err)
	}
	if *resp.ContactInformation.FullName != "山田 太郎" {
		t.Fatalf("expected 山田 太郎, got %s", *resp.ContactInformation.FullName)
	}
}

func TestSeedFromConfigRejectsInvalidContactAndRollsBack(t *testing.T) {
	store := &memory.Store{}
	svc := account.New(store)
	ctx := context.Background()

	cfgs := map[string]config.AccountConfig{
		"111111111111": {
			Name: "Valid Account Before",
			Contact: &config.AccountContactConfig{
				FullName:      "Valid User",
				AddressLine1:  "123 Main St",
				City:          "Seattle",
				StateOrRegion: "WA",
				PostalCode:    "98101",
				CountryCode:   "US",
				PhoneNumber:   "+12065550100",
			},
		},
		"222222222222": {
			Name: "Invalid Account",
			Contact: &config.AccountContactConfig{
				FullName: "Incomplete User",
				// Missing address_line1, city, postal_code, etc.
			},
		},
	}

	err := svc.SeedFromConfig(ctx, cfgs)
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for invalid contact in config, got %v", err)
	}

	// Verify atomicity: account 111111111111 was NOT written because of batch failure
	resp, err := svc.GetAccountInformation(contextWithAccount("111111111111"), nil)
	if err != nil {
		t.Fatalf("unexpected error getting default info: %v", err)
	}
	// Defaults to "TinyStack Account" if no seeded record exists
	if *resp.AccountName != "TinyStack Account" {
		t.Fatalf("expected account 111111111111 to not be modified (default name), got %s", *resp.AccountName)
	}
}

func TestSeedAccountValidation(t *testing.T) {
	svc := account.New(&memory.Store{})
	ctx := context.Background()

	// 1. Invalid AccountId rejected
	err := svc.SeedAccount(ctx, "invalid", "Name", time.Now(), "ACTIVE")
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for invalid AccountId, got %v", err)
	}

	// 2. Invalid state rejected
	err = svc.SeedAccount(ctx, "123456789012", "Name", time.Now(), "INVALID_STATE")
	if err == nil || awserror.From(err).Code != "ValidationException" {
		t.Fatalf("expected ValidationException for invalid AccountState, got %v", err)
	}

	// 3. Valid state accepted
	err = svc.SeedAccount(ctx, "123456789012", "Name", time.Now(), "SUSPENDED")
	if err != nil {
		t.Fatalf("expected valid SeedAccount to succeed, got %v", err)
	}
}
