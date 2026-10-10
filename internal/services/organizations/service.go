// Package organizations implements global organization records. Policies are not enforced.
package organizations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/organizations"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "organizations"
	scp         = "SERVICE_CONTROL_POLICY"
	fullAccess  = "p-FullAWSAccess"
)

// Records: <management-account>/organizations/state. Cross-account membership:
// aws/organizations/account/<account> -> management account. One transaction updates both.
type record struct {
	Resources    map[string]*resource
	Statuses     map[string]*api.CreateAccountStatus
	Organization api.Organization
	Root         api.Root
}
type resource struct {
	Tags    map[string]string
	Account *api.Account
	OU      *api.OrganizationalUnit
	Policy  *api.Policy
	Targets map[string]bool
	ID      string
	Kind    string
	Parent  string
}

type Service struct{ store storage.Store }

var _ api.API = (*Service)(nil)

func New(store storage.Store) *Service { return &Service{store: store} }
func key(account string) string        { return account + "/organizations/state" }
func memberKey(account string) string  { return "aws/organizations/account/" + account }
func invalid(message string) error     { return awserror.New("InvalidInputException", message) }
func (s *Service) read(ctx context.Context, tx storage.ReadTx, management bool) (*record, string, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, "", err
	}

	owner, found, err := storage.Read[string](tx, memberKey(m.AccountID))
	if err != nil {
		return nil, "", err
	}

	if !found {
		return nil, "", awserror.New("AWSOrganizationsNotInUseException", "Account is not in an organization")
	}

	if management && *owner != m.AccountID {
		return nil, "", awserror.New("AccessDeniedException", "Management account required")
	}

	r, found, err := storage.Read[record](tx, key(*owner))
	if err != nil {
		return nil, "", err
	}

	if !found {
		return nil, "", awserror.New("AWSOrganizationsNotInUseException", "Organization not found")
	}

	return r, *owner, nil
}

func (s *Service) view(ctx context.Context, management bool, f func(*record) error) error {
	return s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, txErr := s.read(ctx, tx, management)
		if txErr != nil {
			return txErr
		}

		return f(r)
	})
}

func (s *Service) change(ctx context.Context, f func(*record, storage.WriteTx) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, owner, txErr := s.read(ctx, tx, true)
		if txErr != nil {
			return txErr
		}

		if checkErr := f(r, tx); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, key(owner), r)
	})
}

func resourceARN(r *record, kind, id string) string {
	return fmt.Sprintf(
		"arn:aws:organizations::%s:%s/%s/%s",
		common.Value(r.Organization.MasterAccountId),
		kind,
		common.Value(r.Organization.Id),
		id,
	)
}

func lookup(r *record, id, kind string) (*resource, error) {
	x := r.Resources[id]
	if x != nil && (kind == "" || x.Kind == kind) {
		return x, nil
	}

	code := map[string]string{
		kindAccount: "AccountNotFoundException",
		kindOU:      "OrganizationalUnitNotFoundException",
		kindRoot:    "RootNotFoundException",
		kindPolicy:  "PolicyNotFoundException",
		"":          "TargetNotFoundException",
	}[kind]
	return nil, awserror.New(code, "Resource not found: "+id)
}

func parent(r *record, id string) error {
	x, err := lookup(r, id, "")
	if err != nil {
		return err
	}

	if x.Kind != kindRoot && x.Kind != kindOU {
		return invalid("Parent must be a root or organizational unit")
	}

	return nil
}

func page[T any](items []T, id func(T) string, token *string, maximum *int64) (selected []T, next *string, err error) {
	limit := int(common.Value(common.OrElse(maximum, new(int64(20)))))
	if limit < 1 || limit > 20 {
		return nil, nil, invalid("MaxResults must be 1 through 20")
	}

	return common.Page(items, id, token, limit, invalid("Invalid NextToken"))
}

func tags(in []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		in,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag needs Key and Value"),
		func(string) error { return invalid("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}

	if checkErr := tagging.CheckSet(out); checkErr != nil {
		return nil, invalid(checkErr.Error())
	}

	return out, nil
}

func validName(n string) error {
	if strings.TrimSpace(n) == "" || len(n) > 128 {
		return invalid("Name must contain 1 through 128 characters")
	}

	return nil
}

func policyType(t *string) error {
	if common.Value(t) != scp {
		return awserror.New("NotImplemented", "Only SERVICE_CONTROL_POLICY records are supported")
	}

	return nil
}

func validContent(content *string) error {
	if content == nil || !json.Valid([]byte(*content)) {
		return awserror.New("MalformedPolicyDocumentException", "Content must be JSON")
	}

	return nil
}

func (s *Service) CreateOrganization(
	ctx context.Context,
	in *api.CreateOrganizationRequest,
) (*api.CreateOrganizationResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	feature := common.Value(common.OrElse(in.FeatureSet, new(allFeatures)))
	if feature != allFeatures && feature != "CONSOLIDATED_BILLING" {
		return nil, invalid("Invalid FeatureSet")
	}

	id := "o-" + common.RandomID(10)
	rootID := "r-" + common.RandomID(4)
	r := &record{
		Organization: api.Organization{
			Id:                 new(id),
			Arn:                new("arn:aws:organizations::" + m.AccountID + ":organization/" + id),
			FeatureSet:         new(feature),
			MasterAccountId:    new(m.AccountID),
			MasterAccountEmail: new(m.AccountID + "@tinystack.local"),
		},
		Root:      api.Root{Id: new(rootID), Name: new("Root")},
		Resources: map[string]*resource{},
		Statuses:  map[string]*api.CreateAccountStatus{},
	}

	r.Root.Arn = new(resourceARN(r, kindRoot, rootID))
	r.Organization.MasterAccountArn = new(resourceARN(r, kindAccount, m.AccountID))
	if feature == allFeatures {
		r.Root.PolicyTypes = []api.PolicyTypeSummary{{Type: new(scp), Status: new("ENABLED")}}
		r.Organization.AvailablePolicyTypes = r.Root.PolicyTypes
	}

	r.Resources[rootID] = &resource{ID: rootID, Kind: kindRoot, Tags: map[string]string{}}
	r.Resources[m.AccountID] = &resource{
		ID:     m.AccountID,
		Kind:   kindAccount,
		Parent: rootID,
		Tags:   map[string]string{},
		Account: &api.Account{
			Id:              new(m.AccountID),
			Arn:             r.Organization.MasterAccountArn,
			Email:           r.Organization.MasterAccountEmail,
			Name:            new("Management account"),
			Status:          new("ACTIVE"),
			State:           new("ACTIVE"),
			JoinedMethod:    new("INVITED"),
			JoinedTimestamp: common.SecondsMilli(time.Now()),
		},
	}

	r.Resources[fullAccess] = &resource{
		ID:      fullAccess,
		Kind:    kindPolicy,
		Tags:    map[string]string{},
		Targets: map[string]bool{rootID: true},
		Policy: &api.Policy{
			Content: new(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
			PolicySummary: &api.PolicySummary{
				Id:          new(fullAccess),
				Arn:         new("arn:aws:organizations::aws:policy/service_control_policy/" + fullAccess),
				Name:        new("FullAWSAccess"),
				Description: new("Allows access to every operation"),
				Type:        new(scp),
				AwsManaged:  new(true),
			},
		},
	}

	if feature == allFeatures {
		r.Resources[fullAccess].Targets[m.AccountID] = true
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(memberKey(m.AccountID)); found {
			return awserror.New("AlreadyInOrganizationException", "Account already belongs to an organization")
		}

		if e := storage.Write(tx, key(m.AccountID), r); e != nil {
			return e
		}

		return storage.Write(tx, memberKey(m.AccountID), m.AccountID)
	})
	return &api.CreateOrganizationResponse{Organization: &r.Organization}, err
}

func (s *Service) DescribeOrganization(
	ctx context.Context,
	_ *api.DescribeOrganizationInput,
) (*api.DescribeOrganizationResponse, error) {
	out := &api.DescribeOrganizationResponse{}
	err := s.view(ctx, false, func(r *record) error { out.Organization = &r.Organization; return nil })
	return out, err
}

func (s *Service) DeleteOrganization(
	ctx context.Context,
	_ *api.DeleteOrganizationInput,
) (*api.DeleteOrganizationOutput, error) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, owner, txErr := s.read(ctx, tx, true)
		if txErr != nil {
			return txErr
		}

		for _, x := range r.Resources {
			if x.Kind == kindAccount && x.ID != owner {
				return awserror.New("OrganizationNotEmptyException", "Remove member accounts first")
			}
		}

		tx.Delete(key(owner))
		tx.Delete(memberKey(owner))
		return nil
	})
	return &api.DeleteOrganizationOutput{}, err
}

func (s *Service) CreateAccount(ctx context.Context, in *api.CreateAccountRequest) (*api.CreateAccountResponse, error) {
	if checkErr := validName(common.Value(in.AccountName)); checkErr != nil {
		return nil, checkErr
	}

	if !strings.Contains(common.Value(in.Email), "@") {
		return nil, invalid("Email required")
	}

	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	out := &api.CreateAccountResponse{}
	err = s.change(ctx, func(r *record, tx storage.WriteTx) error {
		for _, x := range r.Resources {
			if x.Account != nil && common.Value(x.Account.Email) == *in.Email {
				return awserror.New("ConstraintViolationException", "Email already exists")
			}
		}

		id := common.RandomID(12, "0123456789")
		for {
			if _, found := tx.Get(memberKey(id)); !found {
				break
			}

			id = common.RandomID(12, "0123456789")
		}

		now := common.SecondsMilli(time.Now())
		status := &api.CreateAccountStatus{
			Id:                 new("car-" + common.RandomID(32)),
			AccountName:        in.AccountName,
			State:              new("SUCCEEDED"),
			AccountId:          new(id),
			RequestedTimestamp: now,
			CompletedTimestamp: now,
		}

		r.Statuses[*status.Id] = status
		r.Resources[id] = &resource{
			ID:     id,
			Kind:   kindAccount,
			Parent: *r.Root.Id,
			Tags:   ts,
			Account: &api.Account{
				Id:              new(id),
				Arn:             new(resourceARN(r, kindAccount, id)),
				Name:            in.AccountName,
				Email:           in.Email,
				Status:          new("ACTIVE"),
				State:           new("ACTIVE"),
				JoinedMethod:    new("CREATED"),
				JoinedTimestamp: now,
			},
		}

		if enabled(r) {
			r.Resources[fullAccess].Targets[id] = true
		}
		out.CreateAccountStatus = status
		return storage.Write(tx, memberKey(id), *r.Organization.MasterAccountId)
	})
	return out, err
}

func (s *Service) DescribeCreateAccountStatus(
	ctx context.Context,
	in *api.DescribeCreateAccountStatusRequest,
) (*api.DescribeCreateAccountStatusResponse, error) {
	out := &api.DescribeCreateAccountStatusResponse{}
	err := s.view(ctx, true, func(r *record) error {
		out.CreateAccountStatus = r.Statuses[common.Value(in.CreateAccountRequestId)]
		if out.CreateAccountStatus == nil {
			return awserror.New("CreateAccountStatusNotFoundException", "Request not found")
		}

		return nil
	})
	return out, err
}

func (s *Service) DescribeAccount(
	ctx context.Context,
	in *api.DescribeAccountRequest,
) (*api.DescribeAccountResponse, error) {
	out := &api.DescribeAccountResponse{}
	err := s.view(ctx, true, func(r *record) error {
		x, txErr := lookup(r, common.Value(in.AccountId), kindAccount)
		if txErr != nil {
			return txErr
		}

		out.Account = x.Account
		return nil
	})
	return out, err
}

func (s *Service) MoveAccount(ctx context.Context, in *api.MoveAccountRequest) (*api.MoveAccountOutput, error) {
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.AccountId), kindAccount)
		if txErr != nil {
			return txErr
		}

		if x.ID == common.Value(r.Organization.MasterAccountId) {
			return awserror.New("ConstraintViolationException", "Cannot move management account")
		}

		if checkErr := parent(r, common.Value(in.DestinationParentId)); checkErr != nil {
			return checkErr
		}

		if x.Parent != common.Value(in.SourceParentId) {
			return awserror.New("SourceParentNotFoundException", "Account is not in source parent")
		}

		if x.Parent == common.Value(in.DestinationParentId) {
			return awserror.New("DuplicateAccountException", "Account already in destination")
		}

		x.Parent = *in.DestinationParentId
		return nil
	})
	return &api.MoveAccountOutput{}, err
}

func (s *Service) RemoveAccountFromOrganization(
	ctx context.Context,
	in *api.RemoveAccountFromOrganizationRequest,
) (*api.RemoveAccountFromOrganizationOutput, error) {
	err := s.change(ctx, func(r *record, tx storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.AccountId), kindAccount)
		if txErr != nil {
			return txErr
		}

		if x.ID == common.Value(r.Organization.MasterAccountId) {
			return awserror.New("ConstraintViolationException", "Cannot remove management account")
		}

		delete(r.Resources, x.ID)
		for _, p := range r.Resources {
			delete(p.Targets, x.ID)
		}

		tx.Delete(memberKey(x.ID))
		return nil
	})
	return &api.RemoveAccountFromOrganizationOutput{}, err
}

const (
	allFeatures = "ALL"
	kindAccount = "account"
	kindPolicy  = "policy"
	kindRoot    = "root"
	kindOU      = "ou"
)
