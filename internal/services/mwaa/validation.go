package mwaa

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,79}$`)
	arnPattern  = regexp.MustCompile(
		`^arn:aws:airflow:([a-z0-9-]+):([0-9]{12}):environment/([A-Za-z][A-Za-z0-9_-]{0,79})$`,
	)
	rolePattern   = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/([A-Za-z0-9_+=,.@/-]+)$`)
	subnetPattern = regexp.MustCompile(`^subnet-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
	groupPattern  = regexp.MustCompile(`^sg-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
)

func defaults(env *api.Environment) {
	if env.EnvironmentClass == nil {
		env.EnvironmentClass = new("mw1.small")
	}
	if env.MinWorkers == nil {
		env.MinWorkers = new(int64(1))
	}
	if env.MaxWorkers == nil {
		env.MaxWorkers = new(int64(10))
	}
	if env.Schedulers == nil {
		env.Schedulers = new(int64(2))
	}
	if env.Tags == nil {
		env.Tags = map[string]string{}
	}
}

func checkTagText(v string, minimum, maximum int) bool {
	n := utf8.RuneCountInString(v)
	if !utf8.ValidString(v) || n < minimum || n > maximum {
		return false
	}
	for _, r := range v {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.Z, r) &&
			!strings.ContainsRune("_.:/=+-@", r) {
			return false
		}
	}
	return true
}

func checkTags(tags map[string]string) error {
	if len(tags) > 50 {
		return invalid("At most 50 tags are supported")
	}
	for k, v := range tags {
		if !checkTagText(k, 1, 128) || strings.HasPrefix(strings.ToLower(k), "aws:") || !checkTagText(v, 1, 256) {
			return invalid(
				"Tags require non-reserved keys 1-128 and values 1-256 Unicode characters: " +
					"letters, numbers, separators or _.:/=+-@",
			)
		}
	}
	return nil
}

func validateConfig(env *api.Environment, scope request.Scope) error {
	if !slices.Contains([]string{"us-east-1", "us-west-2"}, scope.Region) {
		return invalid("This finite records slice supports us-east-1 and us-west-2")
	}
	if !namePattern.MatchString(common.Value(env.Name)) {
		return invalid("Invalid environment Name")
	}
	if !slices.Contains([]string{"2.10.3", "2.11.0"}, common.Value(env.AirflowVersion)) {
		return invalid("AirflowVersion must be explicitly 2.10.3 or 2.11.0; version updates are unsupported")
	}
	if !slices.Contains([]string{"mw1.small", "mw1.medium"}, common.Value(env.EnvironmentClass)) {
		return invalid("EnvironmentClass supports mw1.small or mw1.medium")
	}
	role := common.Value(env.ExecutionRoleArn)
	p := rolePattern.FindStringSubmatch(role)
	if len(p) != 3 || p[1] != scope.AccountID || len(role) > 1224 || strings.Contains(p[2], "//") ||
		strings.HasSuffix(role, "/") {
		return invalid("ExecutionRoleArn requires a commercial IAM role ARN in this account")
	}
	// The pinned MWAA pattern accepts a path; the last component is an IAM role name.
	parts := strings.Split(p[2], "/")
	roleName := parts[len(parts)-1]
	if len(roleName) > 64 || len(p[2])-len(roleName)+1 > 512 {
		return invalid("IAM role name must be at most 64 characters and its path at most 512 characters")
	}
	path := common.Value(env.DagS3Path)
	if !utf8.ValidString(path) || len(path) < 1 || utf8.RuneCountInString(path) > 1024 ||
		strings.HasPrefix(path, "/") ||
		strings.Contains(path, "://") ||
		strings.ContainsAny(path, "\x00\r\n") {
		return invalid("DagS3Path requires a nonempty relative S3 prefix of at most 1024 characters")
	}
	if env.SourceBucketArn == nil || *env.SourceBucketArn == "" {
		return invalid("SourceBucketArn is required")
	}
	if env.NetworkConfiguration == nil || len(env.NetworkConfiguration.SubnetIds) != 2 ||
		len(env.NetworkConfiguration.SecurityGroupIds) < 1 ||
		len(env.NetworkConfiguration.SecurityGroupIds) > 5 {
		return invalid("NetworkConfiguration requires exactly two subnets and 1-5 explicit security groups")
	}
	minimum, maximum, sch := common.Value(env.MinWorkers), common.Value(env.MaxWorkers), common.Value(env.Schedulers)
	if minimum < 1 || maximum < 1 || minimum > maximum || maximum > 25 {
		return invalid("Workers require 1 <= MinWorkers <= MaxWorkers <= 25 locally")
	}
	if sch < 2 || sch > 5 {
		return invalid("Schedulers requires 2-5 for the selected Airflow versions/classes")
	}
	return checkTags(env.Tags)
}

func (s *Service) validateReferences(tx storage.ReadTx, scope request.Scope, env *api.Environment) error {
	if s.network == nil || s.source == nil {
		return invalid("Scoped EC2 and S3 metadata lookup is unavailable")
	}
	if e := s.source.CheckMWAASource(tx, scope, *env.SourceBucketArn); e != nil {
		return invalid(e.Error())
	}
	b := env.NetworkConfiguration
	vpc := ""
	seen := map[string]bool{}
	zones := map[string]bool{}
	zoneIDs := map[string]bool{}
	for _, id := range b.SubnetIds {
		if !subnetPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate SubnetIds")
		}
		seen[id] = true
		sn, e := s.network.MountSubnet(tx, scope, id)
		if e != nil {
			return invalid("SubnetIds must reference existing scoped EC2 metadata")
		}
		if vpc == "" {
			vpc = common.Value(sn.VpcId)
		}
		az, zoneID := common.Value(sn.AvailabilityZone), common.Value(sn.AvailabilityZoneId)
		suffix := strings.TrimPrefix(az, scope.Region)
		if vpc == "" || common.Value(sn.VpcId) != vpc || len(suffix) != 1 || suffix[0] < 'a' || suffix[0] > 'z' ||
			zones[az] ||
			zoneID == "" || zoneIDs[zoneID] ||
			zoneID == "use1-az3" {
			return invalid("Subnets require one VPC, distinct regional AZs and cannot use use1-az3")
		}
		zones[az] = true
		zoneIDs[zoneID] = true
	}
	seen = map[string]bool{}
	for _, id := range b.SecurityGroupIds {
		if !groupPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate SecurityGroupIds")
		}
		seen[id] = true
	}
	if _, e := s.network.MountGroups(tx, scope, vpc, b.SecurityGroupIds); e != nil {
		return invalid("SecurityGroupIds must reference scoped EC2 groups in the subnet VPC")
	}
	return nil
}
