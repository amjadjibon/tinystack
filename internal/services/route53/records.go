package route53

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxChanges    = 1000
	maxRecordSets = 300
	actionCreate  = "CREATE"
	actionDelete  = "DELETE"
	actionUpsert  = "UPSERT"
	typeCNAME     = "CNAME"
	typeA         = "A"
	typeAAAA      = "AAAA"
	typeTXT       = "TXT"
	typeSPF       = "SPF"
	typeMX        = "MX"
	// keySeparator sorts below every character of a stored name, so a name's
	// record sets list before its subdomains'.
	keySeparator = " "
)

var (
	recordTypes = []string{
		typeSOA, typeA, typeTXT, typeNS, typeCNAME, typeMX, "NAPTR", "PTR", "SRV", typeSPF, typeAAAA, "CAA", "DS",
		"TLSA", "SSHFP", "SVCB", "HTTPS",
	}
	quoted = regexp.MustCompile(`^("([^"\\]|\\.)*"\s*)+$`)
	mx     = regexp.MustCompile(`^\d{1,5} \S+$`)
)

func recordPrefix(account, zoneID string) string { return key(account, kindRecord, zoneID+"/") }

func recordKey(account, zoneID string, rs *api.ResourceRecordSet) string {
	return recordPrefix(
		account,
		zoneID,
	) + reversed(
		common.Value(rs.Name),
	) + keySeparator + common.Value(
		rs.Type,
	) + keySeparator +
		common.Value(
			rs.SetIdentifier,
		)
}

func setName(rs *api.ResourceRecordSet) string {
	return fmt.Sprintf("[name='%s', type='%s'%s]", common.Value(rs.Name), common.Value(rs.Type), identifier(rs))
}

func identifier(rs *api.ResourceRecordSet) string {
	if rs.SetIdentifier == nil {
		return ""
	}

	return ", set-identifier='" + *rs.SetIdentifier + "'"
}

// checkRecordSet normalizes a record set's names and checks its shape.
//
//nolint:gocyclo,cyclop // One check per member.
func checkRecordSet(rs *api.ResourceRecordSet) error {
	name, err := normalize(common.Value(rs.Name))
	if err != nil {
		return invalid("Invalid request: %v", err)
	}

	rs.Name = new(name)
	routed := 0
	for _, set := range []bool{
		rs.Weight != nil, rs.Region != nil, rs.GeoLocation != nil, rs.Failover != nil, rs.MultiValueAnswer != nil,
	} {
		if set {
			routed++
		}
	}

	switch t := common.Value(rs.Type); {
	case !slices.Contains(recordTypes, t):
		return invalid("Invalid request: the record type %q is not valid.", t)
	case (rs.AliasTarget == nil) == (rs.TTL == nil || rs.ResourceRecords == nil):
		return invalid("Invalid request: Expected exactly one of [AliasTarget, all of [TTL, and ResourceRecords]] "+
			"in the change for %s.", setName(rs))
	case rs.AliasTarget != nil && (t == typeSOA || t == typeNS):
		return invalid("Invalid request: %s record sets cannot be aliases.", t)
	case rs.TTL != nil && (*rs.TTL < 0 || *rs.TTL > 2147483647):
		return invalid("Invalid request: the TTL of %s must be 0 to 2147483647.", setName(rs))
	case (rs.SetIdentifier != nil) != (routed == 1) || routed > 1:
		return invalid("Invalid request: %s needs a SetIdentifier and exactly one routing policy, or neither.",
			setName(rs))
	case rs.Weight != nil && (*rs.Weight < 0 || *rs.Weight > 255):
		return invalid("Invalid request: the weight must be 0 to 255.")
	case rs.Failover != nil && *rs.Failover != "PRIMARY" && *rs.Failover != "SECONDARY":
		return invalid("Invalid request: Failover must be PRIMARY or SECONDARY.")
	case rs.GeoLocation != nil && rs.GeoLocation.ContinentCode != nil && rs.GeoLocation.CountryCode != nil:
		return invalid("Invalid request: a GeoLocation has a continent or a country, not both.")
	case len(common.Value(rs.SetIdentifier)) > 128:
		return invalid("Invalid request: the SetIdentifier must be at most 128 characters long.")
	}

	if a := rs.AliasTarget; a != nil {
		target, normErr := normalize(common.Value(a.DNSName))
		if normErr != nil || common.Value(a.HostedZoneId) == "" {
			return invalid("Invalid request: the alias target of %s needs a HostedZoneId and a DNSName.", setName(rs))
		}

		a.DNSName, a.HostedZoneId = new(target), new(bareID(a.HostedZoneId))
		return nil
	}

	return checkValues(rs)
}

// checkValues checks the values of a record set that is not an alias.
func checkValues(rs *api.ResourceRecordSet) error {
	t := common.Value(rs.Type)
	if len(rs.ResourceRecords) == 0 || t == typeCNAME && len(rs.ResourceRecords) > 1 {
		return awserror.New("InvalidChangeBatch", fmt.Sprintf("%s must have exactly one value for CNAME records, "+
			"and at least one otherwise.", setName(rs)))
	}

	for _, r := range rs.ResourceRecords {
		v := common.Value(r.Value)
		var ok bool
		switch t {
		case typeA:
			ip := net.ParseIP(v)
			ok = ip != nil && ip.To4() != nil && !strings.Contains(v, ":")
		case typeAAAA:
			ip := net.ParseIP(v)
			ok = ip != nil && strings.Contains(v, ":")
		case typeTXT, typeSPF:
			ok = quoted.MatchString(v)
		case typeMX:
			ok = mx.MatchString(v)
		default:
			ok = strings.TrimSpace(v) != "" && len(v) <= 4000
		}

		if !ok {
			return awserror.New("InvalidChangeBatch",
				fmt.Sprintf("Invalid Resource Record: FATAL problem: the value %q is not valid for type %s", v, t))
		}
	}

	return nil
}

//nolint:gocyclo,cyclop // One case per action and conflict.
func (s *Service) ChangeResourceRecordSets(
	ctx context.Context, in *api.ChangeResourceRecordSetsRequest,
) (*api.ChangeResourceRecordSetsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	batch := in.ChangeBatch
	switch {
	case batch == nil || len(batch.Changes) == 0:
		return nil, invalid("Invalid request: the ChangeBatch needs at least one change.")
	case len(batch.Changes) > maxChanges:
		return nil, invalid("Invalid request: a ChangeBatch can have at most %d changes.", maxChanges)
	}

	for i := range batch.Changes {
		c := &batch.Changes[i]
		a := common.Value(c.Action)
		if a != actionCreate && a != actionDelete && a != actionUpsert || c.ResourceRecordSet == nil {
			return nil, invalid(
				"Invalid request: each change needs a CREATE, DELETE, or UPSERT action and a record set.",
			)
		}

		if checkErr := checkRecordSet(c.ResourceRecordSet); checkErr != nil {
			return nil, checkErr
		}
	}

	id := bareID(in.HostedZoneId)
	out := &api.ChangeResourceRecordSetsResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		z, loadErr := loadZone(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		apex := common.Value(z.Zone.Name)
		var messages []string
		for i := range batch.Changes {
			c := &batch.Changes[i]
			rs := c.ResourceRecordSet
			if msg, checkErr := s.checkChange(tx, m.AccountID, id, apex, common.Value(c.Action), rs); checkErr != nil {
				return checkErr
			} else if msg != "" {
				messages = append(messages, msg)
				continue
			}

			k := recordKey(m.AccountID, id, rs)
			if common.Value(c.Action) == actionDelete {
				tx.Delete(k)
				continue
			}

			if writeErr := storage.Write(tx, k, rs); writeErr != nil {
				return writeErr
			}
		}

		if len(messages) > 0 {
			return awserror.New("InvalidChangeBatch", "["+strings.Join(messages, ", ")+"]")
		}

		z.Zone.ResourceRecordSetCount = new(int64(len(tx.Keys(recordPrefix(m.AccountID, id)))))
		if writeErr := storage.Write(tx, key(m.AccountID, kindZone, id), z); writeErr != nil {
			return writeErr
		}

		var changeErr error
		out.ChangeInfo, changeErr = s.changeInfo(tx, m.AccountID, batch.Comment)
		return changeErr
	})

	return out, err
}

// checkChange returns why a change cannot apply to a zone's current record
// sets, or "" when it can.
//
//nolint:gocyclo,cyclop // One case per action and conflict.
func (s *Service) checkChange(tx storage.ReadTx, account, zoneID, apex, action string, rs *api.ResourceRecordSet,
) (string, error) {
	name, t := common.Value(rs.Name), common.Value(rs.Type)
	if !within(name, apex) {
		return fmt.Sprintf("RRSet with DNS name %s is not permitted in zone %s", name, apex), nil
	}

	k := recordKey(account, zoneID, rs)
	current, found, err := storage.Read[api.ResourceRecordSet](tx, k)
	if err != nil {
		return "", err
	}

	if action == actionDelete {
		switch {
		case !found:
			return fmt.Sprintf("Tried to delete resource record set %s but it was not found", setName(rs)), nil
		case name == apex && (t == typeSOA || t == typeNS):
			return fmt.Sprintf("A HostedZone must contain exactly one %s record set for the zone itself", t), nil
		case !reflect.DeepEqual(current, rs):
			return fmt.Sprintf("Tried to delete resource record set %s but the values provided do not match the "+
				"current values", setName(rs)), nil
		}

		return "", nil
	}

	if found && action == actionCreate {
		return fmt.Sprintf("Tried to create resource record set %s but it already exists", setName(rs)), nil
	}

	switch {
	case t == typeSOA && name != apex:
		return fmt.Sprintf("SOA records are only permitted at the apex of zone %s", apex), nil
	case t == typeCNAME && name == apex:
		return fmt.Sprintf("RRSet of type CNAME with DNS name %s is not permitted at apex in zone %s", name, apex), nil
	}

	// A CNAME excludes other types at its name, and a set without routing
	// excludes routed sets of its name and type.
	byName := recordPrefix(account, zoneID) + reversed(name) + keySeparator
	for _, other := range tx.Keys(byName) {
		if other == k {
			continue
		}

		otherType, otherID, _ := strings.Cut(strings.TrimPrefix(other, byName), keySeparator)
		switch {
		case (t == typeCNAME) != (otherType == typeCNAME):
			return fmt.Sprintf("RRSet of type CNAME with DNS name %s is not permitted as it conflicts with other "+
				"records with the same DNS name in zone %s", name, apex), nil
		case otherType == t && (otherID == "") != (rs.SetIdentifier == nil):
			return fmt.Sprintf("RRSet with DNS name %s, type %s cannot be created because another set with the same "+
				"name and type has a different routing policy", name, t), nil
		}
	}

	if id := rs.HealthCheckId; id != nil {
		if _, ok := tx.Get(key(account, kindHealthCheck, *id)); !ok {
			return "", awserror.New("NoSuchHealthCheck", "No health check exists with the ID "+*id)
		}
	}

	if a := rs.AliasTarget; a != nil {
		target, isZone, targetErr := storage.Read[zone](tx, key(account, kindZone, *a.HostedZoneId))
		if targetErr != nil || !isZone {
			return "", targetErr
		}

		targetName := common.Value(a.DNSName)
		switch {
		case !within(targetName, common.Value(target.Zone.Name)):
			return fmt.Sprintf("Tried to create an alias that targets %s, type %s in zone %s, but the alias target "+
				"name does not lie within the target zone", targetName, t, *a.HostedZoneId), nil
		case len(tx.Keys(recordPrefix(account, *a.HostedZoneId)+reversed(targetName)+keySeparator+t+keySeparator)) == 0:
			return fmt.Sprintf("Tried to create an alias that targets %s, type %s in zone %s, but that target was "+
				"not found", targetName, t, *a.HostedZoneId), nil
		}
	}

	return "", nil
}

func (s *Service) ListResourceRecordSets(
	ctx context.Context, in *api.ListResourceRecordSetsRequest,
) (*api.ListResourceRecordSetsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := maxItems(in.MaxItems, maxRecordSets)
	if err != nil {
		return nil, err
	}

	var start string
	switch {
	case in.StartRecordType != nil && in.StartRecordName == nil:
		return nil, invalid("StartRecordType requires StartRecordName.")
	case in.StartRecordIdentifier != nil && in.StartRecordType == nil:
		return nil, invalid("StartRecordIdentifier requires StartRecordType.")
	case in.StartRecordName != nil:
		name, normErr := normalize(*in.StartRecordName)
		if normErr != nil {
			return nil, invalid("Invalid request: %v", normErr)
		}

		start = reversed(name)
		if in.StartRecordType != nil {
			start += keySeparator + *in.StartRecordType
			if in.StartRecordIdentifier != nil {
				start += keySeparator + *in.StartRecordIdentifier
			}
		}
	}

	id := bareID(in.HostedZoneId)
	out := &api.ListResourceRecordSetsResponse{
		ResourceRecordSets: []api.ResourceRecordSet{}, IsTruncated: new(false), MaxItems: new(limit),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, loadErr := loadZone(tx, m.AccountID, id); loadErr != nil {
			return loadErr
		}

		p := recordPrefix(m.AccountID, id)
		for _, k := range tx.Keys(p) {
			if strings.TrimPrefix(k, p) < start {
				continue
			}

			rs, _, readErr := storage.Read[api.ResourceRecordSet](tx, k)
			if readErr != nil {
				return readErr
			}

			if int64(len(out.ResourceRecordSets)) == limit {
				out.IsTruncated, out.NextRecordName, out.NextRecordType = new(true), rs.Name, rs.Type
				out.NextRecordIdentifier = rs.SetIdentifier
				return nil
			}

			out.ResourceRecordSets = append(out.ResourceRecordSets, *rs)
		}

		return nil
	})

	return out, err
}
