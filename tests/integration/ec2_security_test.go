package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"golang.org/x/crypto/ssh"
)

func tcp(port int32, cidr string) ec2types.IpPermission {
	return ec2types.IpPermission{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int32(port), ToPort: aws.Int32(port),
		IpRanges: []ec2types.IpRange{{CidrIp: aws.String(cidr)}},
	}
}

func TestEC2SecurityGroups(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()

	// Every VPC, the default one included, has a default group.
	groups, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupNames: []string{"default"}})
	if err != nil || len(groups.SecurityGroups) != 1 || len(groups.SecurityGroups[0].IpPermissions) != 1 ||
		len(groups.SecurityGroups[0].IpPermissions[0].UserIdGroupPairs) != 1 ||
		len(groups.SecurityGroups[0].IpPermissionsEgress) != 1 {
		t.Fatalf("got %+v, %v", groups, err)
	}

	defaultGroup := groups.SecurityGroups[0].GroupId
	vpc, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	vpcID := vpc.Vpc.VpcId
	own, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{ec2Filter("vpc-id", aws.ToString(vpcID))},
	})
	if err != nil || len(own.SecurityGroups) != 1 || aws.ToString(own.SecurityGroups[0].GroupName) != "default" {
		t.Fatalf("got %+v, %v", own, err)
	}

	for errCode, in := range map[string]*ec2.CreateSecurityGroupInput{
		"InvalidGroup.Reserved": {GroupName: aws.String("default"), Description: aws.String("d"), VpcId: vpcID},
		"InvalidVpcID.NotFound": {
			GroupName: aws.String("web"), Description: aws.String("d"), VpcId: aws.String("vpc-00000000000000000"),
		},
	} {
		_, err = c.CreateSecurityGroup(ctx, in)
		code(t, err, errCode)
	}

	web, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("web"), Description: aws.String("web servers"), VpcId: vpcID,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSecurityGroup, Tags: ec2Tags("team", "core"),
		}},
	})
	if err != nil ||
		!strings.HasSuffix(aws.ToString(web.SecurityGroupArn), ":security-group/"+aws.ToString(web.GroupId)) {
		t.Fatalf("got %+v, %v", web, err)
	}

	_, err = c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("web"), Description: aws.String("again"), VpcId: vpcID,
	})
	code(t, err, "InvalidGroup.Duplicate")

	db, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("db"), Description: aws.String("databases"), VpcId: vpcID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One rule per source; the same rule twice is a duplicate.
	https := tcp(443, "0.0.0.0/0")
	https.IpRanges = append(https.IpRanges, ec2types.IpRange{CidrIp: aws.String("10.1.0.0/16")})
	added, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: web.GroupId, IpPermissions: []ec2types.IpPermission{https},
	})
	if err != nil || len(added.SecurityGroupRules) != 2 || aws.ToInt32(added.SecurityGroupRules[0].FromPort) != 443 {
		t.Fatalf("got %+v, %v", added, err)
	}

	_, err = c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: web.GroupId, IpPermissions: []ec2types.IpPermission{tcp(443, "0.0.0.0/0")},
	})
	code(t, err, "InvalidPermission.Duplicate")

	for _, perm := range []ec2types.IpPermission{
		tcp(70000, "0.0.0.0/0"),
		tcp(22, "10.0.0.0/33"),
		{IpProtocol: aws.String("carrier-pigeon")},
	} {
		_, err = c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: web.GroupId, IpPermissions: []ec2types.IpPermission{perm},
		})
		code(t, err, "InvalidParameterValue")
	}

	_, err = c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: db.GroupId,
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int32(5432), ToPort: aws.Int32(5432),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: web.GroupId, Description: aws.String("web")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{
		aws.ToString(web.GroupId),
	}})
	if err != nil || len(got.SecurityGroups) != 1 || len(got.SecurityGroups[0].IpPermissions) != 1 ||
		len(got.SecurityGroups[0].IpPermissions[0].IpRanges) != 2 || len(got.SecurityGroups[0].Tags) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}

	byRule, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []ec2types.Filter{
		ec2Filter("ip-permission.group-id", aws.ToString(web.GroupId)),
	}})
	if err != nil || len(byRule.SecurityGroups) != 1 || aws.ToString(byRule.SecurityGroups[0].GroupId) !=
		aws.ToString(db.GroupId) {
		t.Fatalf("got %+v, %v", byRule, err)
	}

	rules, err := c.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{ec2Filter("group-id", aws.ToString(web.GroupId))},
	})
	if err != nil || len(rules.SecurityGroupRules) != 3 {
		t.Fatalf("got %+v, %v", rules, err)
	}

	// Terraform looks for network interfaces left in a group before deleting it.
	enis, err := c.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{ec2Filter("group-id", aws.ToString(web.GroupId))},
	})
	if err != nil || len(enis.NetworkInterfaces) != 0 {
		t.Fatalf("got %+v, %v", enis, err)
	}

	// A group other rules name cannot be deleted, nor can a default group.
	_, err = c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: web.GroupId})
	code(t, err, "DependencyViolation")
	_, err = c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: defaultGroup})
	code(t, err, "CannotDelete")

	// Revoking by permission reports the permissions that matched nothing.
	revoked, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: web.GroupId, IpPermissions: []ec2types.IpPermission{tcp(443, "0.0.0.0/0"), tcp(80, "0.0.0.0/0")},
	})
	if err != nil || len(revoked.UnknownIpPermissions) != 1 {
		t.Fatalf("got %+v, %v", revoked, err)
	}

	_, err = c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: web.GroupId, IpPermissions: []ec2types.IpPermission{tcp(80, "0.0.0.0/0")},
	})
	code(t, err, "InvalidPermission.NotFound")

	dbRules, err := c.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{ec2Filter("group-id", aws.ToString(db.GroupId))},
	})
	if err != nil {
		t.Fatal(err)
	}

	var ingress []string
	for _, r := range dbRules.SecurityGroupRules {
		if !aws.ToBool(r.IsEgress) {
			ingress = append(ingress, aws.ToString(r.SecurityGroupRuleId))
		}
	}

	if _, err = c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: db.GroupId, SecurityGroupRuleIds: ingress,
	}); err != nil {
		t.Fatal(err)
	}

	// A VPC with groups of its own cannot be deleted; its default group goes
	// with it.
	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID})
	code(t, err, "DependencyViolation")
	for _, id := range []*string{web.GroupId, db.GroupId} {
		if _, err = c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: id}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID}); err != nil {
		t.Fatal(err)
	}

	all, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{})
	if err != nil || len(all.SecurityGroups) != 1 {
		t.Fatalf("got %+v, %v", all, err)
	}

	_, err = c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{
		aws.ToString(web.GroupId),
	}})
	code(t, err, "InvalidGroup.NotFound")
}

func TestEC2KeyPairs(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	created, err := c.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{
		KeyName: aws.String("deploy"),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeKeyPair, Tags: ec2Tags("team", "core"),
		}},
	})
	if err != nil || len(aws.ToString(created.KeyFingerprint)) != 59 || len(created.Tags) != 1 {
		t.Fatalf("got %+v, %v", created, err)
	}

	signer, err := ssh.ParsePrivateKey([]byte(aws.ToString(created.KeyMaterial)))
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoRSA {
		t.Fatalf("private key: %v", err)
	}

	_, err = c.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{KeyName: aws.String("deploy")})
	code(t, err, "InvalidKeyPair.Duplicate")

	ed, err := c.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{
		KeyName: aws.String("ed"), KeyType: ec2types.KeyTypeEd25519,
	})
	if err != nil || !strings.HasPrefix(aws.ToString(ed.KeyMaterial), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("got %+v, %v", ed, err)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	imported, err := c.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName: aws.String("laptop"), PublicKeyMaterial: ssh.MarshalAuthorizedKey(sshPub),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName: aws.String("bad"), PublicKeyMaterial: []byte("not a key"),
	})
	code(t, err, "InvalidKey.Format")

	described, err := c.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		KeyNames: []string{"laptop"}, IncludePublicKey: aws.Bool(true),
	})
	if err != nil || len(described.KeyPairs) != 1 || described.KeyPairs[0].KeyType != ec2types.KeyTypeEd25519 ||
		aws.ToString(described.KeyPairs[0].KeyFingerprint) != aws.ToString(imported.KeyFingerprint) ||
		!strings.HasSuffix(aws.ToString(described.KeyPairs[0].PublicKey), " laptop") ||
		described.KeyPairs[0].CreateTime == nil {
		t.Fatalf("got %+v, %v", described, err)
	}

	byType, err := c.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		Filters: []ec2types.Filter{ec2Filter("key-type", "ed25519")},
	})
	if err != nil || len(byType.KeyPairs) != 2 || byType.KeyPairs[0].PublicKey != nil {
		t.Fatalf("got %+v, %v", byType, err)
	}

	_, err = c.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{KeyNames: []string{"missing"}})
	code(t, err, "InvalidKeyPair.NotFound")

	tags, err := c.DescribeTags(ctx, &ec2.DescribeTagsInput{Filters: []ec2types.Filter{
		ec2Filter("resource-type", "key-pair"),
	}})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].ResourceId) != aws.ToString(created.KeyPairId) {
		t.Fatalf("got %+v, %v", tags, err)
	}

	// Deleting is idempotent.
	for range 2 {
		if _, err = c.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}); err != nil {
			t.Fatal(err)
		}
	}

	left, err := c.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{})
	if err != nil || len(left.KeyPairs) != 2 {
		t.Fatalf("got %+v, %v", left, err)
	}
}
