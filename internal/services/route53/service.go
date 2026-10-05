// Package route53 implements Route 53 hosted zones, record sets, and health
// checks without HTTP dependencies. Route 53 is global, so resources belong
// to an account:
//
//	<account>/route53/zone/<zone ID>                      (the zone, its VPCs, name servers, and tags)
//	<account>/route53/rrset/<zone ID>/<sort key>          (one record set)
//	<account>/route53/change/<change ID>
//	<account>/route53/healthcheck/<health check ID>
//	<account>/route53/reference/<kind>/<caller reference> (the resource a caller reference created)
//
// A record set's sort key is its name with the labels reversed, its type,
// and its set identifier, so a zone's record sets list in Route 53's order.
// Changes are applied at once; ChangeResourceRecordSets reports them PENDING
// and GetChange INSYNC. Health checks are stored, never run.
package route53

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service         = "route53"
	kindZone        = "zone"
	kindRecord      = "rrset"
	kindChange      = "change"
	kindHealthCheck = "healthcheck"
	kindReference   = "reference"
	zonePrefix      = "/hostedzone/"
	changePrefix    = "/change/"
	statusPending   = "PENDING"
	statusInSync    = "INSYNC"
	invalidInput    = "InvalidInput"
	maxReference    = 128
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	now    func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// UseCaller lets private hosted zones check their VPCs with EC2. Without it
// VPCs are not checked.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func key(account, kind, name string) string {
	return account + "/" + service + "/" + kind + "/" + name
}

func value[T any](p *T) (v T) { return common.Value(p) }

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	return storage.Read[T](tx, k)
}

func write(tx storage.WriteTx, k string, v any) error { return storage.Write(tx, k, v) }

// newID returns an ID of prefix and uppercase letters and digits, as Route
// 53's zone and change IDs are.
func newID(prefix string, n int) string {
	return prefix + common.RandomID(n, common.AlphabetUpperHex)
}

func invalid(format string, args ...any) error {
	return awserror.New(invalidInput, fmt.Sprintf(format, args...))
}

// bareID drops the /hostedzone/ or /change/ prefix clients may send.
func bareID(id *string) string {
	v := value(id)
	v = strings.TrimPrefix(v, zonePrefix)
	return strings.TrimPrefix(v, changePrefix)
}

// changeInfo records a change and returns it as clients first see it.
func (s *Service) changeInfo(tx storage.WriteTx, account string, comment *string) (*api.ChangeInfo, error) {
	now := s.now().UTC().Truncate(time.Millisecond)
	id := newID("C", 20)
	info := api.ChangeInfo{Id: new(changePrefix + id), Status: new(statusPending), SubmittedAt: &now, Comment: comment}
	if err := write(tx, key(account, kindChange, id), info); err != nil {
		return nil, err
	}

	return &info, nil
}

func (s *Service) GetChange(ctx context.Context, in *api.GetChangeRequest) (*api.GetChangeResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var info *api.ChangeInfo
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		info, found, err = read[api.ChangeInfo](tx, key(m.AccountID, kindChange, bareID(in.Id)))
		if err == nil && !found {
			err = awserror.New("NoSuchChange", "A change with the specified change ID does not exist.")
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	info.Status = new(statusInSync)
	return &api.GetChangeResponse{ChangeInfo: info}, nil
}
