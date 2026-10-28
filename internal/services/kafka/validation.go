package kafka

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	arnPattern  = regexp.MustCompile(
		`^arn:aws:kafka:([a-z0-9-]+):([0-9]{12}):cluster/([A-Za-z0-9_-]{1,64})/` +
			`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:-[0-9]+)?)$`,
	)
	subnetPattern = regexp.MustCompile(`^subnet-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
	groupPattern  = regexp.MustCompile(`^sg-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
)

func checkMonitoring(value *string) error {
	if value == nil ||
		!slices.Contains([]string{"DEFAULT", "PER_BROKER", "PER_TOPIC_PER_BROKER", "PER_TOPIC_PER_PARTITION"}, *value) {
		return invalid(
			"EnhancedMonitoring must be DEFAULT, PER_BROKER, PER_TOPIC_PER_BROKER or PER_TOPIC_PER_PARTITION",
		)
	}

	return nil
}

func checkTags(tags map[string]string) error {
	if e := tagging.CheckSet(tags); e != nil {
		return invalid(e.Error())
	}

	for k, v := range tags {
		if utf8.RuneCountInString(v) > 255 {
			return invalid("Tag values must be 0-255 Unicode characters")
		}

		for _, value := range []string{k, v} {
			for _, r := range value {
				if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsSpace(r) &&
					!strings.ContainsRune("_./=+-@", r) {
					return invalid("Tags require Unicode letters, numbers, whitespace or _ . / = + - @")
				}
			}
		}
	}

	return nil
}

func validateCreate(in *api.CreateClusterRequest, scope request.Scope) error {
	if !slices.Contains([]string{"us-east-1", "us-west-1", "us-west-2"}, scope.Region) {
		return invalid("This finite records slice supports us-east-1, us-west-1 and us-west-2 creation")
	}

	if !namePattern.MatchString(common.Value(in.ClusterName)) {
		return invalid("ClusterName requires 1-64 letters, digits, hyphens or underscores locally")
	}

	if !slices.Contains([]string{"3.8.x", "3.9.x"}, common.Value(in.KafkaVersion)) {
		return invalid("KafkaVersion must be explicitly 3.8.x or 3.9.x (ZooKeeper metadata records)")
	}

	b := in.BrokerNodeGroupInfo
	if b == nil {
		return invalid("BrokerNodeGroupInfo is required")
	}

	if !slices.Contains([]string{"kafka.m5.large", "kafka.m5.xlarge"}, common.Value(b.InstanceType)) {
		return invalid("InstanceType must be kafka.m5.large or kafka.m5.xlarge")
	}

	if len(b.ClientSubnets) < 2 || len(b.ClientSubnets) > 3 ||
		scope.Region == "us-west-1" && len(b.ClientSubnets) != 2 {
		return invalid("ClientSubnets requires 2 or 3 subnets; us-west-1 requires exactly 2")
	}

	n := common.Value(in.NumberOfBrokerNodes)
	if n < 1 || n > 15 || n%int64(len(b.ClientSubnets)) != 0 {
		return invalid(
			"NumberOfBrokerNodes requires 1-15 and an exact multiple of ClientSubnets (pinned model boundary)",
		)
	}

	if b.BrokerAZDistribution == nil {
		b.BrokerAZDistribution = new("DEFAULT")
	}

	if *b.BrokerAZDistribution != "DEFAULT" {
		return invalid("BrokerAZDistribution supports only DEFAULT")
	}

	if len(b.SecurityGroups) < 1 || len(b.SecurityGroups) > 5 {
		return invalid("Explicit SecurityGroups requires 1-5 groups locally; no default group inference")
	}

	if b.StorageInfo == nil || b.StorageInfo.EbsStorageInfo == nil || b.StorageInfo.EbsStorageInfo.VolumeSize == nil {
		return invalid("Explicit StorageInfo.EbsStorageInfo.VolumeSize is required locally")
	}

	size := *b.StorageInfo.EbsStorageInfo.VolumeSize
	if size < 1 || size > 16384 {
		return invalid("VolumeSize must be 1-16384 GiB")
	}

	if in.EnhancedMonitoring == nil {
		in.EnhancedMonitoring = new("DEFAULT")
	}

	if e := checkMonitoring(in.EnhancedMonitoring); e != nil {
		return e
	}

	if e := checkTags(in.Tags); e != nil {
		return e
	}

	if in.Tags == nil {
		in.Tags = map[string]string{}
	}

	return nil
}

func (s *Service) validateNetwork(tx storage.ReadTx, scope request.Scope, b *api.BrokerNodeGroupInfo) error {
	if s.network == nil {
		return invalid("Scoped EC2 metadata lookup is unavailable")
	}

	vpc := ""
	seen := map[string]bool{}
	zones := map[string]bool{}
	for _, id := range b.ClientSubnets {
		if !subnetPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate ClientSubnets")
		}

		seen[id] = true
		sn, e := s.network.MountSubnet(tx, scope, id)
		if e != nil {
			return invalid("ClientSubnets must reference existing scoped EC2 metadata")
		}

		if vpc == "" {
			vpc = common.Value(sn.VpcId)
		}

		az := common.Value(sn.AvailabilityZone)
		zoneID := common.Value(sn.AvailabilityZoneId)
		suffix := strings.TrimPrefix(az, scope.Region)
		if vpc == "" || common.Value(sn.VpcId) != vpc || len(suffix) != 1 || suffix[0] < 'a' || suffix[0] > 'z' ||
			zones[az] ||
			zoneID == "" ||
			zoneID == "use1-az3" {
			return invalid("Subnets require one VPC, distinct regional Availability Zones, and cannot use use1-az3")
		}

		zones[az] = true
	}

	seen = map[string]bool{}
	for _, id := range b.SecurityGroups {
		if !groupPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate SecurityGroups")
		}

		seen[id] = true
	}

	if _, e := s.network.MountGroups(tx, scope, vpc, b.SecurityGroups); e != nil {
		return invalid("SecurityGroups must reference scoped EC2 groups in the subnet VPC")
	}

	return nil
}

// validateAuthentication rejects security combinations MSK cannot expose.
func validateAuthentication(r *api.ClusterInfo) error {
	l := clusterListeners(r)
	encryption := clientTLS
	if r.EncryptionInfo != nil && r.EncryptionInfo.EncryptionInTransit != nil {
		encryption = common.Value(common.OrElse(r.EncryptionInfo.EncryptionInTransit.ClientBroker, new(clientTLS)))
	}

	if encryption == "PLAINTEXT" && (l.scram || l.iam || l.mtls) {
		return invalid("Authenticated listeners require TLS encryption")
	}

	if (l.scram || l.iam) && encryption != clientTLS {
		return invalid("SASL authentication requires ClientBroker TLS")
	}

	if (l.scram || l.iam || l.mtls) && r.EncryptionInfo != nil && r.EncryptionInfo.EncryptionInTransit != nil &&
		r.EncryptionInfo.EncryptionInTransit.InCluster != nil &&
		!*r.EncryptionInfo.EncryptionInTransit.InCluster {
		return invalid("Authenticated listeners require InCluster encryption")
	}

	if !l.plain && !l.tls && !l.scram && !l.iam {
		return invalid("At least one authentication mode must be enabled")
	}

	if r.ClientAuthentication != nil && r.ClientAuthentication.Tls != nil {
		tls := r.ClientAuthentication.Tls
		if len(tls.CertificateAuthorityArnList) != 0 && tls.Enabled != nil && !*tls.Enabled {
			return invalid("CertificateAuthorityArnList requires TLS client authentication")
		}
	}

	return nil
}
