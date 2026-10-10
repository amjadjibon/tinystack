// Package account implements the AWS Account service.
package account

import (
	"context"
	"regexp"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/account"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var accountPattern = regexp.MustCompile(`^\d{12}$`)

type record struct {
	AccountCreatedDate time.Time               `json:"AccountCreatedDate"`
	Contact            *api.ContactInformation `json:"Contact,omitempty"`
	RegionOptStatuses  map[string]string       `json:"RegionOptStatuses,omitempty"`
	AccountID          string                  `json:"AccountId"`
	AccountName        string                  `json:"AccountName"`
	AccountState       string                  `json:"AccountState"`
}

// Service provides AWS Account operations.
type Service struct {
	store storage.Store
}

var _ api.API = (*Service)(nil)

// New creates an AWS Account service backed by store.
func New(store storage.Store) *Service {
	return &Service{store: store}
}

func stateKey(account string) string {
	return account + "/account/state"
}

func defaultRecord(account string) *record {
	return &record{
		AccountID:          account,
		AccountName:        "TinyStack Account",
		AccountCreatedDate: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		AccountState:       "ACTIVE",
		RegionOptStatuses:  make(map[string]string),
	}
}

func (s *Service) resolveAccount(ctx context.Context, requested *string) (string, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return "", err
	}
	if requested == nil {
		return m.AccountID, nil
	}
	if !accountPattern.MatchString(*requested) {
		return "", awserror.Validation("Invalid AccountId")
	}
	if *requested != m.AccountID {
		return "", awserror.New(
			"AccessDeniedException",
			"The operation failed because the calling identity doesn't have the minimum required permissions.",
		)
	}
	return *requested, nil
}

func (s *Service) getRecord(ctx context.Context, account string) (*record, error) {
	var rec *record
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, txErr := storage.Read[record](tx, stateKey(account))
		if txErr != nil {
			return txErr
		}
		if found && r != nil {
			rec = r
			return nil
		}
		rec = defaultRecord(account)
		return nil
	})
	return rec, err
}

func contactInfoToConfig(c *api.ContactInformation) config.AccountContactConfig {
	var cfg config.AccountContactConfig
	if c == nil {
		return cfg
	}
	if c.AddressLine1 != nil {
		cfg.AddressLine1 = *c.AddressLine1
	}
	if c.AddressLine2 != nil {
		cfg.AddressLine2 = *c.AddressLine2
	}
	if c.AddressLine3 != nil {
		cfg.AddressLine3 = *c.AddressLine3
	}
	if c.City != nil {
		cfg.City = *c.City
	}
	if c.CompanyName != nil {
		cfg.CompanyName = *c.CompanyName
	}
	if c.CountryCode != nil {
		cfg.CountryCode = *c.CountryCode
	}
	if c.DistrictOrCounty != nil {
		cfg.DistrictOrCounty = *c.DistrictOrCounty
	}
	if c.FullName != nil {
		cfg.FullName = *c.FullName
	}
	if c.PhoneNumber != nil {
		cfg.PhoneNumber = *c.PhoneNumber
	}
	if c.PostalCode != nil {
		cfg.PostalCode = *c.PostalCode
	}
	if c.StateOrRegion != nil {
		cfg.StateOrRegion = *c.StateOrRegion
	}
	if c.WebsiteUrl != nil {
		cfg.WebsiteURL = *c.WebsiteUrl
	}
	return cfg
}

// SeedFromConfig seeds account metadata, primary contact information, and region overrides
// configured at startup into the store.
func (s *Service) SeedFromConfig(ctx context.Context, accounts map[string]config.AccountConfig) error {
	if len(accounts) == 0 {
		return nil
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		// Pre-validate all accounts before writing any records to keep the transaction atomic.
		for accountID, cfg := range accounts {
			if !accountPattern.MatchString(accountID) {
				return awserror.Validation("Invalid AccountId: %s", accountID)
			}
			if cfg.CreatedDate != "" {
				if _, parseErr := time.Parse(time.RFC3339, cfg.CreatedDate); parseErr != nil {
					return awserror.Validation("Invalid created_date: %v", parseErr)
				}
			}
			if cfg.State != "" && !config.ValidAccountStates[cfg.State] {
				return awserror.Validation("Invalid AccountState: %s", cfg.State)
			}
			if cfg.Contact != nil {
				if err := cfg.Contact.Validate(); err != nil {
					return awserror.Validation("Invalid ContactInformation: %v", err)
				}
			}
			for region, status := range cfg.Regions {
				if !isKnownRegion(region) {
					return awserror.Validation("Invalid RegionName: %s", region)
				}
				if !validRegionOptStatuses[status] {
					return awserror.Validation("Invalid RegionOptStatus: %s", status)
				}
			}
		}

		for accountID, cfg := range accounts {
			r, found, err := storage.Read[record](tx, stateKey(accountID))
			if err != nil {
				return err
			}
			if !found || r == nil {
				r = defaultRecord(accountID)
			}
			if cfg.Name != "" {
				r.AccountName = cfg.Name
			}
			if cfg.CreatedDate != "" {
				t, _ := time.Parse(time.RFC3339, cfg.CreatedDate)
				r.AccountCreatedDate = t
			}
			if cfg.State != "" {
				r.AccountState = cfg.State
			}
			if cfg.Contact != nil {
				contact := &api.ContactInformation{
					AddressLine1: &cfg.Contact.AddressLine1,
					City:         &cfg.Contact.City,
					CountryCode:  &cfg.Contact.CountryCode,
					FullName:     &cfg.Contact.FullName,
					PhoneNumber:  &cfg.Contact.PhoneNumber,
					PostalCode:   &cfg.Contact.PostalCode,
				}
				if cfg.Contact.AddressLine2 != "" {
					contact.AddressLine2 = &cfg.Contact.AddressLine2
				}
				if cfg.Contact.AddressLine3 != "" {
					contact.AddressLine3 = &cfg.Contact.AddressLine3
				}
				if cfg.Contact.CompanyName != "" {
					contact.CompanyName = &cfg.Contact.CompanyName
				}
				if cfg.Contact.DistrictOrCounty != "" {
					contact.DistrictOrCounty = &cfg.Contact.DistrictOrCounty
				}
				if cfg.Contact.StateOrRegion != "" {
					contact.StateOrRegion = &cfg.Contact.StateOrRegion
				}
				if cfg.Contact.WebsiteURL != "" {
					contact.WebsiteUrl = &cfg.Contact.WebsiteURL
				}
				r.Contact = contact
			}
			if len(cfg.Regions) > 0 {
				if r.RegionOptStatuses == nil {
					r.RegionOptStatuses = make(map[string]string)
				}
				for region, status := range cfg.Regions {
					r.RegionOptStatuses[region] = status
				}
			}
			if err := storage.Write(tx, stateKey(accountID), r); err != nil {
				return err
			}
		}
		return nil
	})
}

// SeedContact configures or updates the primary contact information for an account.
func (s *Service) SeedContact(ctx context.Context, account string, contact *api.ContactInformation) error {
	if !accountPattern.MatchString(account) {
		return awserror.Validation("Invalid AccountId: %s", account)
	}
	if contact == nil {
		return awserror.Validation("ContactInformation is required")
	}
	cfg := contactInfoToConfig(contact)
	if err := cfg.Validate(); err != nil {
		return awserror.Validation("Invalid ContactInformation: %v", err)
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[record](tx, stateKey(account))
		if err != nil {
			return err
		}
		if !found || r == nil {
			r = defaultRecord(account)
		}
		r.Contact = contact
		return storage.Write(tx, stateKey(account), r)
	})
}

// SeedAccount configures or updates the account metadata.
func (s *Service) SeedAccount(ctx context.Context, account, name string, created time.Time, state string) error {
	if !accountPattern.MatchString(account) {
		return awserror.Validation("Invalid AccountId: %s", account)
	}
	if state != "" && !config.ValidAccountStates[state] {
		return awserror.Validation("Invalid AccountState: %s", state)
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[record](tx, stateKey(account))
		if err != nil {
			return err
		}
		if !found || r == nil {
			r = defaultRecord(account)
		}
		if name != "" {
			r.AccountName = name
		}
		if !created.IsZero() {
			r.AccountCreatedDate = created
		}
		if state != "" {
			r.AccountState = state
		}
		return storage.Write(tx, stateKey(account), r)
	})
}

// SeedRegionOptStatus sets the region opt-in status for an account.
func (s *Service) SeedRegionOptStatus(ctx context.Context, account, region, status string) error {
	if !accountPattern.MatchString(account) {
		return awserror.Validation("Invalid AccountId: %s", account)
	}
	if !isKnownRegion(region) {
		return awserror.Validation("Invalid RegionName: %s", region)
	}
	if !validRegionOptStatuses[status] {
		return awserror.Validation("Invalid RegionOptStatus: %s", status)
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[record](tx, stateKey(account))
		if err != nil {
			return err
		}
		if !found || r == nil {
			r = defaultRecord(account)
		}
		if r.RegionOptStatuses == nil {
			r.RegionOptStatuses = make(map[string]string)
		}
		r.RegionOptStatuses[region] = status
		return storage.Write(tx, stateKey(account), r)
	})
}

// GetAccountInformation returns account identity, name, creation date, and state.
func (s *Service) GetAccountInformation(
	ctx context.Context,
	in *api.GetAccountInformationRequest,
) (*api.GetAccountInformationResponse, error) {
	if in == nil {
		in = &api.GetAccountInformationRequest{}
	}
	account, err := s.resolveAccount(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}
	rec, err := s.getRecord(ctx, account)
	if err != nil {
		return nil, err
	}
	return &api.GetAccountInformationResponse{
		AccountId:          &rec.AccountID,
		AccountName:        &rec.AccountName,
		AccountCreatedDate: &rec.AccountCreatedDate,
		AccountState:       &rec.AccountState,
	}, nil
}

// GetContactInformation returns primary contact details or ResourceNotFoundException.
func (s *Service) GetContactInformation(
	ctx context.Context,
	in *api.GetContactInformationRequest,
) (*api.GetContactInformationResponse, error) {
	if in == nil {
		in = &api.GetContactInformationRequest{}
	}
	account, err := s.resolveAccount(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}
	rec, err := s.getRecord(ctx, account)
	if err != nil {
		return nil, err
	}
	if rec.Contact == nil {
		return nil, awserror.New(
			"ResourceNotFoundException",
			"Contact information does not exist for the specified account",
		)
	}
	return &api.GetContactInformationResponse{
		ContactInformation: rec.Contact,
	}, nil
}

// GetRegionOptStatus returns the opt-in status for a specific region.
func (s *Service) GetRegionOptStatus(
	ctx context.Context,
	in *api.GetRegionOptStatusRequest,
) (*api.GetRegionOptStatusResponse, error) {
	if in == nil || in.RegionName == nil || *in.RegionName == "" {
		return nil, awserror.Validation("RegionName is required")
	}
	account, err := s.resolveAccount(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}
	region := *in.RegionName
	if !isKnownRegion(region) {
		return nil, awserror.Validation("Invalid RegionName: %s", region)
	}
	rec, err := s.getRecord(ctx, account)
	if err != nil {
		return nil, err
	}
	status := defaultRegionStatus(region)
	if custom, ok := rec.RegionOptStatuses[region]; ok {
		status = custom
	}
	return &api.GetRegionOptStatusResponse{
		RegionName:      &region,
		RegionOptStatus: &status,
	}, nil
}

// ListRegions returns the opt-in status for all commercial regions,
// optionally filtered by opt-in status.
func (s *Service) ListRegions(
	ctx context.Context,
	in *api.ListRegionsRequest,
) (*api.ListRegionsResponse, error) {
	if in == nil {
		in = &api.ListRegionsRequest{}
	}
	account, err := s.resolveAccount(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}
	for _, status := range in.RegionOptStatusContains {
		if !validRegionOptStatuses[status] {
			return nil, awserror.Validation("Invalid RegionOptStatus: %s", status)
		}
	}

	rec, err := s.getRecord(ctx, account)
	if err != nil {
		return nil, err
	}

	var all []api.Region
	for _, reg := range allRegions {
		status := defaultRegionStatus(reg)
		if custom, ok := rec.RegionOptStatuses[reg]; ok {
			status = custom
		}
		if len(in.RegionOptStatusContains) > 0 && !slices.Contains(in.RegionOptStatusContains, status) {
			continue
		}
		rName := reg
		all = append(all, api.Region{
			RegionName:      &rName,
			RegionOptStatus: &status,
		})
	}

	limit := 50
	if in.MaxResults != nil {
		if *in.MaxResults < 1 || *in.MaxResults > 50 {
			return nil, awserror.Validation("MaxResults must be between 1 and 50")
		}
		limit = int(*in.MaxResults)
	}

	page, next, err := common.Page(
		all,
		func(r api.Region) string { return *r.RegionName },
		in.NextToken,
		limit,
		awserror.Validation("Invalid NextToken"),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListRegionsResponse{
		Regions:   page,
		NextToken: next,
	}, nil
}
