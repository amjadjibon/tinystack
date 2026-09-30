package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

func route53Client(s *httptest.Server) *route53.Client {
	return route53.New(route53.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func change(action r53types.ChangeAction, name string, t r53types.RRType, values ...string) r53types.Change {
	rs := &r53types.ResourceRecordSet{Name: aws.String(name), Type: t, TTL: aws.Int64(300)}
	for _, v := range values {
		rs.ResourceRecords = append(rs.ResourceRecords, r53types.ResourceRecord{Value: aws.String(v)})
	}

	return r53types.Change{Action: action, ResourceRecordSet: rs}
}

func changeRecords(c *route53.Client, zone *string, changes ...r53types.Change) error {
	_, err := c.ChangeResourceRecordSets(context.Background(), &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: zone, ChangeBatch: &r53types.ChangeBatch{Changes: changes},
	})
	return err
}

func TestRoute53Records(t *testing.T) {
	c := route53Client(server(t))
	ctx := context.Background()
	created, err := c.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: aws.String("Example.com"), CallerReference: aws.String("one"),
		HostedZoneConfig: &r53types.HostedZoneConfig{Comment: aws.String("main")},
	})
	if err != nil {
		t.Fatal(err)
	}

	zone := created.HostedZone
	if !strings.HasPrefix(aws.ToString(zone.Id), "/hostedzone/Z") || aws.ToString(zone.Name) != "example.com." ||
		len(created.DelegationSet.NameServers) != 4 || created.ChangeInfo.Status != r53types.ChangeStatusPending ||
		!strings.HasSuffix(aws.ToString(created.Location), strings.TrimPrefix(aws.ToString(zone.Id), "/hostedzone/")) {
		t.Fatalf("created: %+v %+v %+v", zone, created.DelegationSet, created.ChangeInfo)
	}

	synced, err := c.GetChange(ctx, &route53.GetChangeInput{Id: created.ChangeInfo.Id})
	if err != nil || synced.ChangeInfo.Status != r53types.ChangeStatusInsync {
		t.Fatalf("change: %+v %v", synced, err)
	}

	var exists *r53types.HostedZoneAlreadyExists
	if _, err = c.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: aws.String("other.com"), CallerReference: aws.String("one"),
	}); !errors.As(err, &exists) {
		t.Fatalf("reused caller reference: %v", err)
	}

	weighted := func(id string, weight int64, ip string) r53types.Change {
		ch := change(r53types.ChangeActionCreate, "api.example.com", r53types.RRTypeA, ip)
		ch.ResourceRecordSet.SetIdentifier, ch.ResourceRecordSet.Weight = aws.String(id), aws.Int64(weight)
		return ch
	}

	err = changeRecords(c, zone.Id,
		change(r53types.ChangeActionCreate, "www.example.com", r53types.RRTypeA, "10.0.0.1", "10.0.0.2"),
		change(r53types.ChangeActionCreate, "example.com", r53types.RRTypeTxt, `"v=spf1 -all"`),
		change(r53types.ChangeActionCreate, "*.example.com", r53types.RRTypeCname, "www.example.com"),
		weighted("blue", 90, "10.0.1.1"), weighted("green", 10, "10.0.1.2"),
	)
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{HostedZoneId: zone.Id})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, rs := range listed.ResourceRecordSets {
		got = append(got, aws.ToString(rs.Name)+" "+string(rs.Type)+" "+aws.ToString(rs.SetIdentifier))
	}

	want := "example.com. NS |example.com. SOA |example.com. TXT |\\052.example.com. CNAME |" +
		"api.example.com. A blue|api.example.com. A green|www.example.com. A "
	if strings.Join(got, "|") != want {
		t.Fatalf("record sets:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}

	page, err := c.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: zone.Id, StartRecordName: aws.String("api.example.com"), MaxItems: aws.Int32(1),
	})
	if err != nil || len(page.ResourceRecordSets) != 1 || !page.IsTruncated ||
		aws.ToString(page.NextRecordIdentifier) != "green" {
		t.Fatalf("page: %+v %v", page, err)
	}

	var batch *r53types.InvalidChangeBatch
	for name, ch := range map[string]r53types.Change{
		"duplicate": change(r53types.ChangeActionCreate, "www.example.com", r53types.RRTypeA, "10.0.0.9"),
		"mismatched delete": change(r53types.ChangeActionDelete, "www.example.com", r53types.RRTypeA,
			"10.0.0.1"),
		"CNAME conflict":  change(r53types.ChangeActionCreate, "www.example.com", r53types.RRTypeCname, "x.com"),
		"outside zone":    change(r53types.ChangeActionCreate, "www.other.com", r53types.RRTypeA, "10.0.0.1"),
		"unquoted TXT":    change(r53types.ChangeActionCreate, "txt.example.com", r53types.RRTypeTxt, "plain"),
		"bad IPv4":        change(r53types.ChangeActionCreate, "bad.example.com", r53types.RRTypeA, "::1"),
		"apex NS removal": change(r53types.ChangeActionDelete, "example.com", r53types.RRTypeNs, "x"),
	} {
		if err = changeRecords(c, zone.Id, ch); !errors.As(err, &batch) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	alias := r53types.Change{Action: r53types.ChangeActionUpsert, ResourceRecordSet: &r53types.ResourceRecordSet{
		Name: aws.String("app.example.com"), Type: r53types.RRTypeA,
		AliasTarget: &r53types.AliasTarget{
			HostedZoneId: zone.Id, DNSName: aws.String("www.example.com"), EvaluateTargetHealth: false,
		},
	}}
	if err = changeRecords(c, zone.Id, alias); err != nil {
		t.Fatal(err)
	}

	var notEmpty *r53types.HostedZoneNotEmpty
	if _, err = c.DeleteHostedZone(ctx, &route53.DeleteHostedZoneInput{Id: zone.Id}); !errors.As(err, &notEmpty) {
		t.Fatalf("deleting a zone with records: %v", err)
	}

	byName, err := c.ListHostedZonesByName(ctx, &route53.ListHostedZonesByNameInput{DNSName: aws.String("example.com")})
	if err != nil || len(byName.HostedZones) != 1 || aws.ToInt64(byName.HostedZones[0].ResourceRecordSetCount) != 8 {
		t.Fatalf("by name: %+v %v", byName, err)
	}

	blue, green := weighted("blue", 90, "10.0.1.1"), weighted("green", 10, "10.0.1.2")
	blue.Action, green.Action = r53types.ChangeActionDelete, r53types.ChangeActionDelete
	err = changeRecords(c, zone.Id,
		r53types.Change{Action: r53types.ChangeActionDelete, ResourceRecordSet: alias.ResourceRecordSet},
		change(r53types.ChangeActionDelete, "www.example.com", r53types.RRTypeA, "10.0.0.1", "10.0.0.2"),
		change(r53types.ChangeActionDelete, "example.com", r53types.RRTypeTxt, `"v=spf1 -all"`),
		change(r53types.ChangeActionDelete, "*.example.com", r53types.RRTypeCname, "www.example.com"),
		blue, green,
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteHostedZone(ctx, &route53.DeleteHostedZoneInput{Id: zone.Id}); err != nil {
		t.Fatal(err)
	}

	var noZone *r53types.NoSuchHostedZone
	if _, err = c.GetHostedZone(ctx, &route53.GetHostedZoneInput{Id: zone.Id}); !errors.As(err, &noZone) {
		t.Fatalf("deleted zone: %v", err)
	}
}

func TestRoute53PrivateZonesHealthChecksAndTags(t *testing.T) {
	s := server(t)
	c := route53Client(s)
	ctx := context.Background()
	vpcs, err := ec2Client(s, "test", "us-east-1").DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil || len(vpcs.Vpcs) == 0 {
		t.Fatalf("%+v %v", vpcs, err)
	}

	vpc := &r53types.VPC{VPCId: vpcs.Vpcs[0].VpcId, VPCRegion: r53types.VPCRegionUsEast1}
	private, err := c.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: aws.String("internal"), CallerReference: aws.String("private"), VPC: vpc,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetHostedZone(ctx, &route53.GetHostedZoneInput{Id: private.HostedZone.Id})
	if err != nil || !got.HostedZone.Config.PrivateZone || len(got.VPCs) != 1 || got.DelegationSet != nil {
		t.Fatalf("private zone: %+v %v", got, err)
	}

	var invalidVPC *r53types.InvalidVPCId
	if _, err = c.CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: aws.String("other.internal"), CallerReference: aws.String("bad"),
		VPC: &r53types.VPC{VPCId: aws.String("vpc-00000000"), VPCRegion: r53types.VPCRegionUsEast1},
	}); !errors.As(err, &invalidVPC) {
		t.Fatalf("missing VPC: %v", err)
	}

	var last *r53types.LastVPCAssociation
	if _, err = c.DisassociateVPCFromHostedZone(ctx, &route53.DisassociateVPCFromHostedZoneInput{
		HostedZoneId: private.HostedZone.Id, VPC: vpc,
	}); !errors.As(err, &last) {
		t.Fatalf("removing the last VPC: %v", err)
	}

	check, err := c.CreateHealthCheck(ctx, &route53.CreateHealthCheckInput{
		CallerReference: aws.String("web"),
		HealthCheckConfig: &r53types.HealthCheckConfig{
			Type: r53types.HealthCheckTypeHttps, FullyQualifiedDomainName: aws.String("www.example.com"),
			ResourcePath: aws.String("/health"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := check.HealthCheck.HealthCheckConfig
	if aws.ToInt32(cfg.Port) != 443 || aws.ToInt32(cfg.RequestInterval) != 30 || !aws.ToBool(cfg.EnableSNI) {
		t.Fatalf("health check defaults: %+v", cfg)
	}

	id := check.HealthCheck.Id
	updated, err := c.UpdateHealthCheck(ctx, &route53.UpdateHealthCheckInput{
		HealthCheckId: id, HealthCheckVersion: aws.Int64(1), FailureThreshold: aws.Int32(5),
		ResetElements: []r53types.ResettableElementName{r53types.ResettableElementNameResourcePath},
	})
	if err != nil || aws.ToInt64(updated.HealthCheck.HealthCheckVersion) != 2 ||
		updated.HealthCheck.HealthCheckConfig.ResourcePath != nil {
		t.Fatalf("update: %+v %v", updated, err)
	}

	var mismatch *r53types.HealthCheckVersionMismatch
	if _, err = c.UpdateHealthCheck(ctx, &route53.UpdateHealthCheckInput{
		HealthCheckId: id, HealthCheckVersion: aws.Int64(1),
	}); !errors.As(err, &mismatch) {
		t.Fatalf("stale version: %v", err)
	}

	_, err = c.ChangeTagsForResource(ctx, &route53.ChangeTagsForResourceInput{
		ResourceType: r53types.TagResourceTypeHealthcheck, ResourceId: id,
		AddTags: []r53types.Tag{{Key: aws.String("team"), Value: aws.String("web")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &route53.ListTagsForResourceInput{
		ResourceType: r53types.TagResourceTypeHealthcheck, ResourceId: id,
	})
	if err != nil || len(tags.ResourceTagSet.Tags) != 1 {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	ch := change(r53types.ChangeActionCreate, "db.internal", r53types.RRTypeA, "10.0.0.5")
	ch.ResourceRecordSet.HealthCheckId = id
	ch.ResourceRecordSet.SetIdentifier = aws.String("primary")
	ch.ResourceRecordSet.Failover = r53types.ResourceRecordSetFailoverPrimary
	if err = changeRecords(c, private.HostedZone.Id, ch); err != nil {
		t.Fatal(err)
	}

	var inUse *r53types.HealthCheckInUse
	if _, err = c.DeleteHealthCheck(ctx, &route53.DeleteHealthCheckInput{HealthCheckId: id}); !errors.As(err, &inUse) {
		t.Fatalf("deleting a health check in use: %v", err)
	}

	count, err := c.GetHostedZoneCount(ctx, &route53.GetHostedZoneCountInput{})
	if err != nil || aws.ToInt64(count.HostedZoneCount) != 1 {
		t.Fatalf("count: %+v %v", count, err)
	}
}
