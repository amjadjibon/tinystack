package route53

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultZones   = 100
	defaultTTL     = 172800
	soaTTL         = 900
	typeSOA        = "SOA"
	typeNS         = "NS"
	privateZone    = "PrivateHostedZone"
	zoneLocation   = "https://route53.amazonaws.com/2013-04-01/hostedzone/"
	noSuchZoneCode = "NoSuchHostedZone"
)

// zone is a hosted zone's record.
type zone struct {
	Zone        api.HostedZone
	Tags        map[string]string
	VPCs        []api.VPC `json:",omitempty"`
	NameServers []string  `json:",omitempty"`
}

func noSuchZone(id string) error {
	return awserror.New(noSuchZoneCode, "No hosted zone found with ID: "+id)
}

// loadZone reads the zone id names.
func loadZone(tx storage.ReadTx, account, id string) (*zone, error) {
	z, found, err := storage.Read[zone](tx, key(account, kindZone, id))
	if err == nil && !found {
		err = noSuchZone(id)
	}

	return z, err
}

// nameServers are the four name servers a zone is delegated to.
func nameServers(id string) []string {
	n := 0
	for _, c := range id {
		n = n*31 + int(c)
	}

	n = max(n, -n) % 2048
	return []string{
		fmt.Sprintf("ns-%d.awsdns-%02d.com.", n, n%64),
		fmt.Sprintf("ns-%d.awsdns-%02d.net.", (n+512)%2048, (n+16)%64),
		fmt.Sprintf("ns-%d.awsdns-%02d.org.", (n+1024)%2048, (n+32)%64),
		fmt.Sprintf("ns-%d.awsdns-%02d.co.uk.", (n+1536)%2048, (n+48)%64),
	}
}

// checkVPC asks EC2 whether a VPC exists in its Region.
func (s *Service) checkVPC(ctx context.Context, account string, vpc *api.VPC) error {
	if vpc == nil || common.Value(vpc.VPCId) == "" || common.Value(vpc.VPCRegion) == "" {
		return awserror.New("InvalidVPCId", "A VPC needs a VPCId and a VPCRegion.")
	}

	if s.caller == nil {
		return nil
	}

	ctx = request.WithMetadata(ctx, request.Metadata{
		Scope: request.Scope{AccountID: account, Region: *vpc.VPCRegion}, Service: "ec2",
	})
	_, err := s.caller.Call(ctx, "ec2", "DescribeVpcs", map[string]any{"VpcIds": []any{*vpc.VPCId}})
	if err != nil {
		return awserror.New("InvalidVPCId", "The VPC ID "+*vpc.VPCId+" is not valid in "+*vpc.VPCRegion+".")
	}

	return nil
}

func (s *Service) CreateHostedZone(
	ctx context.Context, in *api.CreateHostedZoneRequest,
) (*api.CreateHostedZoneResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	name, err := normalize(common.Value(in.Name))
	if err != nil {
		return nil, awserror.New("InvalidDomainName", err.Error())
	}

	ref := common.Value(in.CallerReference)
	private := in.VPC != nil
	config := &api.HostedZoneConfig{PrivateZone: new(private)}
	if c := in.HostedZoneConfig; c != nil {
		config.Comment = c.Comment
		if common.Value(c.PrivateZone) && !private {
			return nil, invalid("A private hosted zone needs a VPC.")
		}
	}

	switch {
	case ref == "" || len(ref) > maxReference:
		return nil, invalid("The CallerReference must be 1 to 128 characters long.")
	case strings.Count(name, ".") < 2 && !private:
		return nil, awserror.New("InvalidDomainName", name+" is reserved by AWS.")
	case len(common.Value(config.Comment)) > 256:
		return nil, invalid("The comment must be at most 256 characters long.")
	}

	if private {
		if checkErr := s.checkVPC(ctx, m.AccountID, in.VPC); checkErr != nil {
			return nil, checkErr
		}
	}

	id := newID("Z", 20)
	z := &zone{
		Zone: api.HostedZone{
			Id: new(zonePrefix + id), Name: new(name), CallerReference: new(ref), Config: config,
			ResourceRecordSetCount: new(int64(2)),
		},
		NameServers: nameServers(id),
		Tags:        map[string]string{},
	}
	if private {
		z.VPCs = []api.VPC{*in.VPC}
	}

	out := &api.CreateHostedZoneResponse{HostedZone: &z.Zone, Location: new(zoneLocation + id)}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		refKey := key(m.AccountID, kindReference, kindZone+"/"+ref)
		if _, taken := tx.Get(refKey); taken {
			return awserror.New("HostedZoneAlreadyExists",
				"A hosted zone has already been created with the specified caller reference.")
		}

		tx.Put(refKey, []byte(`"`+id+`"`))
		if writeErr := storage.Write(tx, key(m.AccountID, kindZone, id), z); writeErr != nil {
			return writeErr
		}

		for _, rs := range apexRecords(name, z.NameServers) {
			if writeErr := storage.Write(tx, recordKey(m.AccountID, id, &rs), rs); writeErr != nil {
				return writeErr
			}
		}

		var changeErr error
		out.ChangeInfo, changeErr = s.changeInfo(tx, m.AccountID, nil)
		return changeErr
	})
	if err != nil {
		return nil, err
	}

	if private {
		out.VPC = in.VPC
	} else {
		out.DelegationSet = &api.DelegationSet{NameServers: z.NameServers}
	}

	return out, nil
}

// apexRecords are the SOA and NS record sets every zone starts with.
func apexRecords(name string, servers []string) []api.ResourceRecordSet {
	ns := make([]api.ResourceRecord, len(servers))
	for i, server := range servers {
		ns[i] = api.ResourceRecord{Value: new(server)}
	}

	soa := servers[0] + " awsdns-hostmaster.amazon.com. 1 7200 900 1209600 86400"
	return []api.ResourceRecordSet{
		{Name: new(name), Type: new(typeNS), TTL: new(int64(defaultTTL)), ResourceRecords: ns},
		{
			Name: new(name), Type: new(typeSOA), TTL: new(int64(soaTTL)),
			ResourceRecords: []api.ResourceRecord{{Value: new(soa)}},
		},
	}
}

func (s *Service) GetHostedZone(ctx context.Context, in *api.GetHostedZoneRequest) (*api.GetHostedZoneResponse, error) {
	z, err := s.viewZone(ctx, bareID(in.Id))
	if err != nil {
		return nil, err
	}

	out := &api.GetHostedZoneResponse{HostedZone: &z.Zone}
	if common.Value(z.Zone.Config.PrivateZone) {
		out.VPCs = z.VPCs
	} else {
		out.DelegationSet = &api.DelegationSet{NameServers: z.NameServers}
	}

	return out, nil
}

func (s *Service) viewZone(ctx context.Context, id string) (*zone, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var z *zone
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		z, err = loadZone(tx, m.AccountID, id)
		return err
	})

	return z, err
}

// zones reads every hosted zone of an account, in ID order.
func (s *Service) zones(ctx context.Context) ([]*zone, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var out []*zone
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(m.AccountID, kindZone, "")) {
			z, _, readErr := storage.Read[zone](tx, k)
			if readErr != nil {
				return readErr
			}

			out = append(out, z)
		}

		return nil
	})

	return out, err
}

func maxItems(n *int64, limit int64) (int64, error) {
	if n == nil {
		return limit, nil
	}

	if *n < 1 {
		return 0, invalid("MaxItems must be a positive number.")
	}

	return min(*n, limit), nil
}

func (s *Service) ListHostedZones(
	ctx context.Context, in *api.ListHostedZonesRequest,
) (*api.ListHostedZonesResponse, error) {
	limit, err := maxItems(in.MaxItems, defaultZones)
	if err != nil {
		return nil, err
	}

	if t := in.HostedZoneType; t != nil && *t != privateZone {
		return nil, invalid("HostedZoneType must be PrivateHostedZone.")
	}

	all, err := s.zones(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListHostedZonesResponse{
		HostedZones: []api.HostedZone{},
		Marker:      new(common.Value(in.Marker)),
		IsTruncated: new(false),
		MaxItems:    new(limit),
	}
	for _, z := range all {
		id := bareID(z.Zone.Id)
		switch {
		case in.Marker != nil && id < *in.Marker,
			in.HostedZoneType != nil && !common.Value(z.Zone.Config.PrivateZone):
			continue
		}

		if int64(len(out.HostedZones)) == limit {
			out.IsTruncated, out.NextMarker = new(true), new(id)
			break
		}

		out.HostedZones = append(out.HostedZones, z.Zone)
	}

	return out, nil
}

func (s *Service) ListHostedZonesByName(
	ctx context.Context, in *api.ListHostedZonesByNameRequest,
) (*api.ListHostedZonesByNameResponse, error) {
	limit, err := maxItems(in.MaxItems, defaultZones)
	if err != nil {
		return nil, err
	}

	var start string
	if in.DNSName != nil {
		if start, err = normalize(*in.DNSName); err != nil {
			return nil, awserror.New("InvalidDomainName", err.Error())
		}
	} else if in.HostedZoneId != nil {
		return nil, invalid("HostedZoneId requires DNSName.")
	}

	all, err := s.zones(ctx)
	if err != nil {
		return nil, err
	}

	slices.SortStableFunc(all, func(a, b *zone) int {
		if c := strings.Compare(reversed(common.Value(a.Zone.Name)), reversed(common.Value(b.Zone.Name))); c != 0 {
			return c
		}

		return strings.Compare(common.Value(a.Zone.Id), common.Value(b.Zone.Id))
	})

	out := &api.ListHostedZonesByNameResponse{
		HostedZones: []api.HostedZone{}, DNSName: in.DNSName, HostedZoneId: in.HostedZoneId,
		IsTruncated: new(false), MaxItems: new(limit),
	}
	for _, z := range all {
		name, id := common.Value(z.Zone.Name), bareID(z.Zone.Id)
		if c := strings.Compare(reversed(name), reversed(start)); start != "" &&
			(c < 0 || c == 0 && in.HostedZoneId != nil && id < bareID(in.HostedZoneId)) {
			continue
		}

		if int64(len(out.HostedZones)) == limit {
			out.IsTruncated, out.NextDNSName, out.NextHostedZoneId = new(true), new(name), new(id)
			break
		}

		out.HostedZones = append(out.HostedZones, z.Zone)
	}

	return out, nil
}

func (s *Service) GetHostedZoneCount(
	ctx context.Context, _ *api.GetHostedZoneCountRequest,
) (*api.GetHostedZoneCountResponse, error) {
	all, err := s.zones(ctx)
	if err != nil {
		return nil, err
	}

	return &api.GetHostedZoneCountResponse{HostedZoneCount: new(int64(len(all)))}, nil
}

func (s *Service) DeleteHostedZone(
	ctx context.Context, in *api.DeleteHostedZoneRequest,
) (*api.DeleteHostedZoneResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	id := bareID(in.Id)
	out := &api.DeleteHostedZoneResponse{}
	return out, s.store.Update(ctx, func(tx storage.WriteTx) error {
		z, loadErr := loadZone(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		records := tx.Keys(key(m.AccountID, kindRecord, id+"/"))
		for _, k := range records {
			rs, _, readErr := storage.Read[api.ResourceRecordSet](tx, k)
			if readErr != nil {
				return readErr
			}

			if t := common.Value(
				rs.Type,
			); common.Value(rs.Name) != common.Value(z.Zone.Name) ||
				t != typeSOA && t != typeNS {
				return awserror.New("HostedZoneNotEmpty",
					"The specified hosted zone contains non-required resource record sets and so cannot be deleted.")
			}
		}

		for _, k := range records {
			tx.Delete(k)
		}

		tx.Delete(key(m.AccountID, kindZone, id))
		tx.Delete(key(m.AccountID, kindReference, kindZone+"/"+common.Value(z.Zone.CallerReference)))
		var changeErr error
		out.ChangeInfo, changeErr = s.changeInfo(tx, m.AccountID, nil)
		return changeErr
	})
}

func (s *Service) UpdateHostedZoneComment(
	ctx context.Context, in *api.UpdateHostedZoneCommentRequest,
) (*api.UpdateHostedZoneCommentResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if len(common.Value(in.Comment)) > 256 {
		return nil, invalid("The comment must be at most 256 characters long.")
	}

	id := bareID(in.Id)
	out := &api.UpdateHostedZoneCommentResponse{}
	return out, s.store.Update(ctx, func(tx storage.WriteTx) error {
		z, loadErr := loadZone(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		z.Zone.Config.Comment = in.Comment
		out.HostedZone = &z.Zone
		return storage.Write(tx, key(m.AccountID, kindZone, id), z)
	})
}

func (s *Service) AssociateVPCWithHostedZone(
	ctx context.Context, in *api.AssociateVPCWithHostedZoneRequest,
) (*api.AssociateVPCWithHostedZoneResponse, error) {
	out := &api.AssociateVPCWithHostedZoneResponse{}
	var err error
	out.ChangeInfo, err = s.associate(ctx, bareID(in.HostedZoneId), in.VPC, in.Comment, true)
	return out, err
}

func (s *Service) DisassociateVPCFromHostedZone(
	ctx context.Context, in *api.DisassociateVPCFromHostedZoneRequest,
) (*api.DisassociateVPCFromHostedZoneResponse, error) {
	out := &api.DisassociateVPCFromHostedZoneResponse{}
	var err error
	out.ChangeInfo, err = s.associate(ctx, bareID(in.HostedZoneId), in.VPC, in.Comment, false)
	return out, err
}

// associate adds a VPC to, or removes one from, a private hosted zone.
func (s *Service) associate(ctx context.Context, id string, vpc *api.VPC, comment *string, add bool,
) (*api.ChangeInfo, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if add {
		if checkErr := s.checkVPC(ctx, m.AccountID, vpc); checkErr != nil {
			return nil, checkErr
		}
	} else if vpc == nil {
		return nil, invalid("The VPC is required.")
	}

	var info *api.ChangeInfo
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		z, loadErr := loadZone(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		if !common.Value(z.Zone.Config.PrivateZone) {
			return awserror.New("PublicZoneVPCAssociation", "Public hosted zones cannot be associated with VPCs.")
		}

		i := slices.IndexFunc(z.VPCs, func(v api.VPC) bool {
			return common.Value(v.VPCId) == common.Value(vpc.VPCId) &&
				common.Value(v.VPCRegion) == common.Value(vpc.VPCRegion)
		})
		switch {
		case add && i >= 0:
			return awserror.New("ConflictingDomainExists", "The VPC "+common.Value(vpc.VPCId)+" is already associated.")
		case add:
			z.VPCs = append(z.VPCs, *vpc)
		case i < 0:
			return awserror.New("VPCAssociationNotFound", "The VPC "+common.Value(vpc.VPCId)+" is not associated.")
		case len(z.VPCs) == 1:
			return awserror.New("LastVPCAssociation", "A private hosted zone needs at least one VPC.")
		default:
			z.VPCs = slices.Delete(z.VPCs, i, i+1)
		}

		if writeErr := storage.Write(tx, key(m.AccountID, kindZone, id), z); writeErr != nil {
			return writeErr
		}

		var changeErr error
		info, changeErr = s.changeInfo(tx, m.AccountID, comment)
		return changeErr
	})

	return info, err
}

func (s *Service) GetDNSSEC(ctx context.Context, in *api.GetDNSSECRequest) (*api.GetDNSSECResponse, error) {
	if _, err := s.viewZone(ctx, bareID(in.HostedZoneId)); err != nil {
		return nil, err
	}

	return &api.GetDNSSECResponse{
		Status:         &api.DNSSECStatus{ServeSignature: new("NOT_SIGNING")},
		KeySigningKeys: []api.KeySigningKey{},
	}, nil
}
