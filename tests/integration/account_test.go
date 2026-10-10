package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/account"
	"github.com/aws/aws-sdk-go-v2/service/account/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func accountClient(s, key string) *account.Client {
	return account.New(account.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s),
		Retryer:      aws.NopRetryer{},
		Credentials:  creds(key),
	})
}

func TestAccountSDKWorkflow(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := accountClient(s.URL, "test")

	// 1. GetAccountInformation default caller
	info, err := c.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("GetAccountInformation failed: %v", err)
	}
	if aws.ToString(info.AccountId) != "000000000000" {
		t.Fatalf("expected AccountId 000000000000, got %s", aws.ToString(info.AccountId))
	}
	if aws.ToString(info.AccountName) != "TinyStack Account" {
		t.Fatalf("expected AccountName TinyStack Account, got %s", aws.ToString(info.AccountName))
	}
	if info.AccountState != types.AccountStateActive {
		t.Fatalf("expected AccountState ACTIVE, got %v", info.AccountState)
	}
	if info.AccountCreatedDate == nil || info.AccountCreatedDate.IsZero() {
		t.Fatalf("expected non-zero AccountCreatedDate")
	}

	// 2. Explicit matching AccountId succeeds
	infoMatching, err := c.GetAccountInformation(ctx, &account.GetAccountInformationInput{
		AccountId: aws.String("000000000000"),
	})
	if err != nil {
		t.Fatalf("GetAccountInformation with matching AccountId failed: %v", err)
	}
	if aws.ToString(infoMatching.AccountId) != "000000000000" {
		t.Fatalf("expected AccountId 000000000000, got %s", aws.ToString(infoMatching.AccountId))
	}

	// 3. Foreign AccountId returns AccessDeniedException
	_, err = c.GetAccountInformation(ctx, &account.GetAccountInformationInput{
		AccountId: aws.String("111111111111"),
	})
	code(t, err, "AccessDeniedException")

	// 4. Malformed AccountId returns ValidationException
	_, err = c.GetAccountInformation(ctx, &account.GetAccountInformationInput{
		AccountId: aws.String("invalid-account"),
	})
	code(t, err, "ValidationException")

	// 5. GetContactInformation with no contact returns ResourceNotFoundException
	_, err = c.GetContactInformation(ctx, &account.GetContactInformationInput{})
	code(t, err, "ResourceNotFoundException")

	// 6. GetRegionOptStatus for standard region
	optStatus, err := c.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("us-east-1"),
	})
	if err != nil {
		t.Fatalf("GetRegionOptStatus failed: %v", err)
	}
	if optStatus.RegionOptStatus != types.RegionOptStatusEnabledByDefault {
		t.Fatalf("expected ENABLED_BY_DEFAULT, got %v", optStatus.RegionOptStatus)
	}

	// 7. GetRegionOptStatus for opt-in region
	optStatusOptIn, err := c.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("af-south-1"),
	})
	if err != nil {
		t.Fatalf("GetRegionOptStatus opt-in failed: %v", err)
	}
	if optStatusOptIn.RegionOptStatus != types.RegionOptStatusDisabled {
		t.Fatalf("expected DISABLED, got %v", optStatusOptIn.RegionOptStatus)
	}

	// 8. GetRegionOptStatus for unknown region
	_, err = c.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("unknown-region-99"),
	})
	code(t, err, "ValidationException")

	// 9. ListRegions returns all regions
	regions, err := c.ListRegions(ctx, &account.ListRegionsInput{})
	if err != nil {
		t.Fatalf("ListRegions failed: %v", err)
	}
	if len(regions.Regions) != 28 {
		t.Fatalf("expected 28 regions, got %d", len(regions.Regions))
	}

	// 10. ListRegions filtered by status
	disabledRegions, err := c.ListRegions(ctx, &account.ListRegionsInput{
		RegionOptStatusContains: []types.RegionOptStatus{types.RegionOptStatusDisabled},
	})
	if err != nil {
		t.Fatalf("ListRegions filtered failed: %v", err)
	}
	if len(disabledRegions.Regions) == 0 {
		t.Fatalf("expected at least 1 disabled region")
	}
	for _, r := range disabledRegions.Regions {
		if r.RegionOptStatus != types.RegionOptStatusDisabled {
			t.Fatalf("unexpected region status %v for %s", r.RegionOptStatus, aws.ToString(r.RegionName))
		}
	}

	// 11. ListRegions pagination
	var allPaged []string
	var token *string
	for {
		page, pageErr := c.ListRegions(ctx, &account.ListRegionsInput{
			MaxResults: aws.Int32(5),
			NextToken:  token,
		})
		if pageErr != nil {
			t.Fatalf("pagination step failed: %v", pageErr)
		}
		for _, reg := range page.Regions {
			allPaged = append(allPaged, aws.ToString(reg.RegionName))
		}
		if page.NextToken == nil || *page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	if len(allPaged) != 28 {
		t.Fatalf("expected 28 regions paginated, got %d", len(allPaged))
	}

	// 12. ListRegions invalid parameters
	_, err = c.ListRegions(ctx, &account.ListRegionsInput{
		MaxResults: aws.Int32(0),
	})
	code(t, err, "ValidationException")

	_, err = c.ListRegions(ctx, &account.ListRegionsInput{
		NextToken: aws.String("bad-token"),
	})
	code(t, err, "ValidationException")
}

func TestAccountRawRESTJSONWire(t *testing.T) {
	s := server(t)
	client := s.Client()

	post := func(path string, body []byte) (*http.Response, []byte) {
		req, err := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		auth := "AWS4-HMAC-SHA256 Credential=test/20240101/us-east-1/account/aws4_request, " +
			"SignedHeaders=host, Signature=dummy"
		req.Header.Set("Authorization", auth)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp, respBody
	}

	// 1. POST /getAccountInformation with empty JSON
	resp, body := post("/getAccountInformation", []byte("{}"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var acctInfo map[string]any
	if err := json.Unmarshal(body, &acctInfo); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if acctInfo["AccountId"] != "000000000000" {
		t.Fatalf("expected AccountId 000000000000, got %v", acctInfo["AccountId"])
	}
	if acctInfo["AccountState"] != "ACTIVE" {
		t.Fatalf("expected AccountState ACTIVE, got %v", acctInfo["AccountState"])
	}

	// 2. POST /getContactInformation with empty JSON -> 404 ResourceNotFoundException
	respNotFound, bodyNotFound := post("/getContactInformation", []byte("{}"))
	if respNotFound.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", respNotFound.StatusCode, bodyNotFound)
	}

	// 3. POST /getRegionOptStatus with valid region -> 200
	respRegion, bodyRegion := post("/getRegionOptStatus", []byte(`{"RegionName":"us-east-1"}`))
	if respRegion.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", respRegion.StatusCode, bodyRegion)
	}
	var regionInfo map[string]any
	if err := json.Unmarshal(bodyRegion, &regionInfo); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if regionInfo["RegionOptStatus"] != "ENABLED_BY_DEFAULT" {
		t.Fatalf("expected ENABLED_BY_DEFAULT, got %v", regionInfo["RegionOptStatus"])
	}

	// 4. POST /getRegionOptStatus with invalid region -> 400 ValidationException
	respInvalidRegion, bodyInvalid := post("/getRegionOptStatus", []byte(`{"RegionName":"not-a-region"}`))
	if respInvalidRegion.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", respInvalidRegion.StatusCode, bodyInvalid)
	}

	// 5. POST /getAccountInformation with foreign AccountId -> 403 AccessDeniedException
	respDenied, bodyDenied := post("/getAccountInformation", []byte(`{"AccountId":"111111111111"}`))
	if respDenied.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", respDenied.StatusCode, bodyDenied)
	}

	// 6. POST /getAccountInformation with malformed AccountId -> 400 ValidationException
	respBadAcct, bodyBadAcct := post("/getAccountInformation", []byte(`{"AccountId":"invalid"}`))
	if respBadAcct.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", respBadAcct.StatusCode, bodyBadAcct)
	}

	// 7. POST /getAccountInformation with explicit empty AccountId -> 400 ValidationException
	respEmptyAcct, bodyEmptyAcct := post("/getAccountInformation", []byte(`{"AccountId":""}`))
	if respEmptyAcct.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty AccountId, got %d: %s", respEmptyAcct.StatusCode, bodyEmptyAcct)
	}

	// 8. POST /getContactInformation with explicit empty AccountId -> 400 ValidationException
	respEmptyContact, bodyEmptyContact := post("/getContactInformation", []byte(`{"AccountId":""}`))
	if respEmptyContact.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty AccountId, got %d: %s", respEmptyContact.StatusCode, bodyEmptyContact)
	}

	// 9. POST /getRegionOptStatus with explicit empty AccountId -> 400 ValidationException
	respEmptyRegion, bodyEmptyRegion := post("/getRegionOptStatus", []byte(`{"AccountId":"","RegionName":"us-east-1"}`))
	if respEmptyRegion.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty AccountId, got %d: %s", respEmptyRegion.StatusCode, bodyEmptyRegion)
	}
}

func TestAccountPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Initial instance with configured custom metadata, contact, and region overrides
	store1, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg1 := testConfig()
	cfg1.Storage.Mode, cfg1.Storage.Path = config.StoragePersistent, dir
	cfg1.Accounts = map[string]config.AccountConfig{
		"000000000000": {
			Name:        "Custom Persisted Account",
			CreatedDate: "2023-08-10T14:00:00Z",
			State:       "ACTIVE",
			Contact: &config.AccountContactConfig{
				FullName:      "Persisted Contact",
				AddressLine1:  "500 Persist Ave",
				City:          "Austin",
				StateOrRegion: "TX",
				PostalCode:    "78701",
				CountryCode:   "US",
				PhoneNumber:   "+15125550199",
				CompanyName:   "PersistCo",
			},
			Regions: map[string]string{
				"af-south-1": "ENABLED",
			},
		},
	}
	s1, stop1 := serverStore(t, cfg1, &fakeRuntime{}, store1)
	c1 := accountClient(s1.URL, "test")

	infoBefore, err := c1.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("initial GetAccountInformation failed: %v", err)
	}
	if aws.ToString(infoBefore.AccountName) != "Custom Persisted Account" {
		t.Fatalf("expected Custom Persisted Account, got %s", aws.ToString(infoBefore.AccountName))
	}
	contactBefore, err := c1.GetContactInformation(ctx, &account.GetContactInformationInput{})
	if err != nil {
		t.Fatalf("initial GetContactInformation failed: %v", err)
	}
	if aws.ToString(contactBefore.ContactInformation.FullName) != "Persisted Contact" {
		t.Fatalf("expected Persisted Contact, got %s", aws.ToString(contactBefore.ContactInformation.FullName))
	}
	regionBefore, err := c1.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("af-south-1"),
	})
	if err != nil {
		t.Fatalf("initial GetRegionOptStatus failed: %v", err)
	}
	if regionBefore.RegionOptStatus != types.RegionOptStatusEnabled {
		t.Fatalf("expected ENABLED, got %v", regionBefore.RegionOptStatus)
	}

	stop1()
	if closeErr := store1.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	// Restart server against same directory with EMPTY cfg.Accounts (removed startup seed)
	store2, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := testConfig()
	cfg2.Storage.Mode, cfg2.Storage.Path = config.StoragePersistent, dir
	// cfg2.Accounts is nil/empty!
	s2, stop2 := serverStore(t, cfg2, &fakeRuntime{}, store2)
	defer func() {
		stop2()
		_ = store2.Close()
	}()
	c2 := accountClient(s2.URL, "test")

	infoAfter, err := c2.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("post-restart GetAccountInformation failed: %v", err)
	}
	if aws.ToString(infoAfter.AccountName) != "Custom Persisted Account" {
		t.Fatalf("expected custom account name preserved from storage, got %s", aws.ToString(infoAfter.AccountName))
	}
	expectedTime, _ := time.Parse(time.RFC3339, "2023-08-10T14:00:00Z")
	if !infoAfter.AccountCreatedDate.Equal(expectedTime) {
		t.Fatalf("expected created date %v preserved from storage, got %v", expectedTime, infoAfter.AccountCreatedDate)
	}

	contactAfter, err := c2.GetContactInformation(ctx, &account.GetContactInformationInput{})
	if err != nil {
		t.Fatalf("post-restart GetContactInformation failed: %v", err)
	}
	if aws.ToString(contactAfter.ContactInformation.FullName) != "Persisted Contact" {
		t.Fatalf(
			"expected contact preserved from storage, got %s",
			aws.ToString(contactAfter.ContactInformation.FullName),
		)
	}
	if aws.ToString(contactAfter.ContactInformation.City) != "Austin" {
		t.Fatalf("expected city preserved from storage, got %s", aws.ToString(contactAfter.ContactInformation.City))
	}

	regionAfter, err := c2.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("af-south-1"),
	})
	if err != nil {
		t.Fatalf("post-restart GetRegionOptStatus failed: %v", err)
	}
	if regionAfter.RegionOptStatus != types.RegionOptStatusEnabled {
		t.Fatalf("expected af-south-1 ENABLED preserved from storage, got %v", regionAfter.RegionOptStatus)
	}

	// Another account remains separate / default
	cOther := accountClient(s2.URL, "team-b") // account 111111111111
	infoOther, err := cOther.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("other account GetAccountInformation failed: %v", err)
	}
	if aws.ToString(infoOther.AccountName) != "TinyStack Account" {
		t.Fatalf("expected default name for other account, got %s", aws.ToString(infoOther.AccountName))
	}
	_, err = cOther.GetContactInformation(ctx, &account.GetContactInformationInput{})
	code(t, err, "ResourceNotFoundException")
}

func TestAccountConfiguredWorkflow(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Accounts = map[string]config.AccountConfig{
		"000000000000": {
			Name:        "Configured Corp",
			CreatedDate: "2023-03-15T09:30:00Z",
			State:       "ACTIVE",
			Contact: &config.AccountContactConfig{
				FullName:      "Jane Doe",
				CompanyName:   "Configured Corp",
				AddressLine1:  "123 Innovation Way",
				City:          "San Francisco",
				StateOrRegion: "CA",
				PostalCode:    "94105",
				CountryCode:   "US",
				PhoneNumber:   "+14155550199",
				WebsiteURL:    "https://example.com",
			},
			Regions: map[string]string{
				"af-south-1": "ENABLED",
				"eu-south-1": "ENABLED",
			},
		},
	}
	s := serverConfig(t, cfg, &fakeRuntime{})
	client := accountClient(s.URL, "test")

	// 1. SDK GetAccountInformation
	info, err := client.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("GetAccountInformation failed: %v", err)
	}
	if aws.ToString(info.AccountName) != "Configured Corp" {
		t.Fatalf("expected Configured Corp, got %s", aws.ToString(info.AccountName))
	}
	expectedDate, _ := time.Parse(time.RFC3339, "2023-03-15T09:30:00Z")
	if !info.AccountCreatedDate.Equal(expectedDate) {
		t.Fatalf("expected %v, got %v", expectedDate, info.AccountCreatedDate)
	}

	// 2. SDK GetContactInformation
	contact, err := client.GetContactInformation(ctx, &account.GetContactInformationInput{})
	if err != nil {
		t.Fatalf("GetContactInformation failed: %v", err)
	}
	if aws.ToString(contact.ContactInformation.FullName) != "Jane Doe" {
		t.Fatalf("expected Jane Doe, got %s", aws.ToString(contact.ContactInformation.FullName))
	}
	if aws.ToString(contact.ContactInformation.CompanyName) != "Configured Corp" {
		t.Fatalf("expected Configured Corp, got %s", aws.ToString(contact.ContactInformation.CompanyName))
	}
	if aws.ToString(contact.ContactInformation.City) != "San Francisco" {
		t.Fatalf("expected San Francisco, got %s", aws.ToString(contact.ContactInformation.City))
	}
	if aws.ToString(contact.ContactInformation.WebsiteUrl) != "https://example.com" {
		t.Fatalf("expected https://example.com, got %s", aws.ToString(contact.ContactInformation.WebsiteUrl))
	}

	// 3. SDK Region overrides
	optStatus, err := client.GetRegionOptStatus(ctx, &account.GetRegionOptStatusInput{
		RegionName: aws.String("af-south-1"),
	})
	if err != nil {
		t.Fatalf("GetRegionOptStatus failed: %v", err)
	}
	if optStatus.RegionOptStatus != types.RegionOptStatusEnabled {
		t.Fatalf("expected ENABLED, got %v", optStatus.RegionOptStatus)
	}

	// 4. Raw REST/JSON wire verification
	httpClient := s.Client()
	post := func(path string, body []byte) (*http.Response, []byte) {
		req, postErr := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(body))
		if postErr != nil {
			t.Fatal(postErr)
		}
		req.Header.Set("Content-Type", "application/json")
		auth := "AWS4-HMAC-SHA256 Credential=test/20240101/us-east-1/account/aws4_request, " +
			"SignedHeaders=host, Signature=dummy"
		req.Header.Set("Authorization", auth)
		resp, doErr := httpClient.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		defer resp.Body.Close()
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp, respBody
	}

	resp, body := post("/getContactInformation", []byte("{}"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for raw getContactInformation, got %d: %s", resp.StatusCode, body)
	}
	var rawContact map[string]any
	if unmarshalErr := json.Unmarshal(body, &rawContact); unmarshalErr != nil {
		t.Fatalf("unmarshal contact failed: %v", unmarshalErr)
	}
	ci, ok := rawContact["ContactInformation"].(map[string]any)
	if !ok || ci["FullName"] != "Jane Doe" {
		t.Fatalf("unexpected raw contact body: %v", rawContact)
	}

	// 5. Foreign-account isolation: team-b (111111111111) has no contact configured
	clientB := accountClient(s.URL, "team-b")
	_, err = clientB.GetContactInformation(ctx, &account.GetContactInformationInput{})
	code(t, err, "ResourceNotFoundException")

	// team-b cannot query 000000000000's contact
	_, err = clientB.GetContactInformation(ctx, &account.GetContactInformationInput{
		AccountId: aws.String("000000000000"),
	})
	code(t, err, "AccessDeniedException")
}

func TestAccountMultiAccountIsolation(t *testing.T) {
	s := server(t)
	ctx := context.Background()

	clientA := accountClient(s.URL, "test")   // account 000000000000
	clientB := accountClient(s.URL, "team-b") // account 111111111111

	infoA, err := clientA.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("clientA info failed: %v", err)
	}
	if aws.ToString(infoA.AccountId) != "000000000000" {
		t.Fatalf("clientA unexpected account: %s", aws.ToString(infoA.AccountId))
	}

	infoB, err := clientB.GetAccountInformation(ctx, &account.GetAccountInformationInput{})
	if err != nil {
		t.Fatalf("clientB info failed: %v", err)
	}
	if aws.ToString(infoB.AccountId) != "111111111111" {
		t.Fatalf("clientB unexpected account: %s", aws.ToString(infoB.AccountId))
	}

	// Client A cannot query Client B's account
	_, err = clientA.GetAccountInformation(ctx, &account.GetAccountInformationInput{
		AccountId: aws.String("111111111111"),
	})
	code(t, err, "AccessDeniedException")

	// Client B cannot query Client A's account
	_, err = clientB.GetAccountInformation(ctx, &account.GetAccountInformationInput{
		AccountId: aws.String("000000000000"),
	})
	code(t, err, "AccessDeniedException")
}

func TestAccountSigV4VerifyMode(t *testing.T) {
	s := verifyingServer(t)
	signer := v4.NewSigner()
	client := s.Client()

	now := time.Now()
	body := []byte("{}")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	// 1. Valid signature with secret "test"
	req, err := http.NewRequest(http.MethodPost, s.URL+"/getAccountInformation", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signErr := signer.SignHTTP(
		context.Background(),
		aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"},
		req,
		payloadHash,
		"account",
		"us-east-1",
		now,
	); signErr != nil {
		t.Fatal(signErr)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 with valid signature, got %d: %s", resp.StatusCode, respBody)
	}

	// 2. Invalid secret
	reqWrong, err := http.NewRequest(http.MethodPost, s.URL+"/getAccountInformation", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	reqWrong.Header.Set("Content-Type", "application/json")
	if signErr := signer.SignHTTP(
		context.Background(),
		aws.Credentials{AccessKeyID: "wrong", SecretAccessKey: "wrong-secret"},
		reqWrong,
		payloadHash,
		"account",
		"us-east-1",
		now,
	); signErr != nil {
		t.Fatal(signErr)
	}

	respWrong, err := client.Do(reqWrong)
	if err != nil {
		t.Fatal(err)
	}
	defer respWrong.Body.Close()
	if respWrong.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with wrong signature, got %d", respWrong.StatusCode)
	}
}
