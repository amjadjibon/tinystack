package mq

import (
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,50}$`)
	idPattern       = regexp.MustCompile(`^b-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.~\-]{2,100}$`)
	tokenPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	timePattern     = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)
	subnetPattern   = regexp.MustCompile(`^subnet-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
	groupPattern    = regexp.MustCompile(`^sg-[0-9a-f]{8}(?:[0-9a-f]{9})?$`)
)

func defaultWindow() *api.WeeklyStartTime {
	return &api.WeeklyStartTime{DayOfWeek: new("MONDAY"), TimeOfDay: new("00:00"), TimeZone: new("UTC")}
}

func validateWindow(w *api.WeeklyStartTime) error {
	if !slices.Contains([]string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"},
		common.Value(w.DayOfWeek)) || !timePattern.MatchString(common.Value(w.TimeOfDay)) {
		return invalid("Maintenance window requires a day of week and HH:MM time")
	}
	if w.TimeZone == nil {
		w.TimeZone = new("UTC")
	}
	if *w.TimeZone != "UTC" {
		return invalid("This records slice supports only the UTC maintenance time zone")
	}
	return nil
}

func validateConfig(b *api.DescribeBrokerResponse, runtime bool) error {
	engine, version, host, mode := common.Value(b.EngineType), common.Value(b.EngineVersion),
		common.Value(b.HostInstanceType), common.Value(b.DeploymentMode)
	if host != micro && host != large {
		return invalid("Supported hostInstanceType values are mq.t3.micro and mq.m5.large")
	}
	switch engine {
	case activeMQ:
		if !runtime && version != "5.18" && version != "5.19" {
			return invalid("ActiveMQ engineVersion must be explicitly 5.18 or 5.19")
		}
		if mode != single && mode != standby {
			return invalid("ActiveMQ supports SINGLE_INSTANCE or ACTIVE_STANDBY_MULTI_AZ")
		}
	case rabbitMQ:
		if !runtime && version != "3.13" {
			return invalid("RabbitMQ engineVersion must be explicitly 3.13")
		}
		if mode != single && mode != cluster {
			return invalid("RabbitMQ supports SINGLE_INSTANCE or CLUSTER_MULTI_AZ")
		}
	default:
		return invalid("EngineType must be ACTIVEMQ or RABBITMQ")
	}
	if runtime {
		if _, err := brokerImage(engine, version); err != nil {
			return invalid(err.Error())
		}
	}

	if host == micro && mode != single {
		return invalid("mq.t3.micro requires SINGLE_INSTANCE")
	}
	if b.PubliclyAccessible == nil {
		return invalid("PubliclyAccessible is required")
	}
	if b.AutoMinorVersionUpgrade == nil {
		b.AutoMinorVersionUpgrade = new(true)
	}
	if !*b.AutoMinorVersionUpgrade {
		return invalid("Selected engine versions require AutoMinorVersionUpgrade=true")
	}
	if b.EncryptionOptions == nil {
		b.EncryptionOptions = &api.EncryptionOptions{UseAwsOwnedKey: new(true)}
	}
	if b.EncryptionOptions.UseAwsOwnedKey == nil || !*b.EncryptionOptions.UseAwsOwnedKey {
		return invalid("EncryptionOptions supports only explicit UseAwsOwnedKey=true")
	}
	if b.Logs == nil {
		b.Logs = &api.LogsSummary{}
	}
	if engine == rabbitMQ && b.Logs.Audit != nil {
		return invalid("Audit logging does not apply to RabbitMQ, including false")
	}
	if b.Logs.General == nil {
		b.Logs.General = new(false)
	}
	if engine == activeMQ && b.Logs.Audit == nil {
		b.Logs.Audit = new(false)
	}
	if b.MaintenanceWindowStartTime == nil {
		b.MaintenanceWindowStartTime = defaultWindow()
	}
	if e := validateWindow(b.MaintenanceWindowStartTime); e != nil {
		return e
	}
	if e := tagging.CheckSet(b.Tags); e != nil {
		return invalid(e.Error())
	}
	if b.Tags == nil {
		b.Tags = map[string]string{}
	}
	return nil
}

func validateCreate(in *api.CreateBrokerRequest, runtime bool) error {
	if !namePattern.MatchString(common.Value(in.BrokerName)) {
		return invalid("BrokerName requires 1-50 letters, digits, hyphens or underscores")
	}
	if in.CreatorRequestId != nil && !tokenPattern.MatchString(*in.CreatorRequestId) {
		return invalid("CreatorRequestId requires 1-128 letters, digits, hyphens or underscores locally")
	}
	if len(in.Users) < 1 || len(in.Users) > 250 || common.Value(in.EngineType) == rabbitMQ && len(in.Users) != 1 {
		return invalid("Create requires 1-250 ActiveMQ users or exactly one RabbitMQ administrator")
	}
	seen := map[string]bool{}
	for _, u := range in.Users {
		if common.Value(in.EngineType) == rabbitMQ && (u.ConsoleAccess != nil || u.Groups != nil) {
			return invalid("RabbitMQ administrator does not support ConsoleAccess or Groups inputs")
		}

		name := common.Value(u.Username)
		if !usernamePattern.MatchString(name) || seen[name] ||
			common.Value(in.EngineType) == rabbitMQ && (name == "guest" || strings.Contains(name, "~")) {
			return invalid("Invalid or duplicate initial username")
		}
		seen[name] = true
		pw := common.Value(u.Password)
		unique := map[byte]bool{}
		for i := range len(pw) {
			if pw[i] < 32 || pw[i] > 126 {
				return invalid("Initial password requires printable ASCII characters")
			}
			unique[pw[i]] = true
		}
		if len(pw) < 12 || len(pw) > 250 || len(unique) < 4 || strings.ContainsAny(pw, ",:=") {
			return invalid(
				"Initial password requires 12-250 printable characters, four unique characters, and excludes comma/colon/equals",
			)
		}
	}
	b := &api.DescribeBrokerResponse{
		EngineType:                 in.EngineType,
		EngineVersion:              in.EngineVersion,
		HostInstanceType:           in.HostInstanceType,
		DeploymentMode:             in.DeploymentMode,
		PubliclyAccessible:         in.PubliclyAccessible,
		AutoMinorVersionUpgrade:    in.AutoMinorVersionUpgrade,
		EncryptionOptions:          in.EncryptionOptions,
		MaintenanceWindowStartTime: in.MaintenanceWindowStartTime,
		Tags:                       in.Tags,
	}
	if in.Logs != nil {
		b.Logs = &api.LogsSummary{General: in.Logs.General, Audit: in.Logs.Audit}
	}
	if e := validateConfig(b, runtime); e != nil {
		return e
	}
	// Compatibility validation also accepts retained legacy records. New RabbitMQ
	// brokers cannot use the deprecated micro instance, even in single mode.
	if *b.EngineType == rabbitMQ && *b.HostInstanceType == micro {
		return invalid("mq.t3.micro is unavailable for new RabbitMQ brokers")
	}
	in.AutoMinorVersionUpgrade = b.AutoMinorVersionUpgrade
	in.EncryptionOptions = b.EncryptionOptions
	in.Logs = &api.Logs{General: b.Logs.General, Audit: b.Logs.Audit}
	in.MaintenanceWindowStartTime = b.MaintenanceWindowStartTime
	in.Tags = b.Tags
	return nil
}

func (s *Service) validateNetwork(tx storage.ReadTx, scope request.Scope, b *api.DescribeBrokerResponse) error {
	// Public brokers without references use AWS-managed networking in AWS. Locally
	// this records the choice and allocates nothing. Explicit public networking is
	// deliberately excluded rather than inventing a default VPC or security group.
	if *b.PubliclyAccessible {
		if b.SubnetIds != nil || b.SecurityGroups != nil {
			return invalid("Explicit subnet/security-group references require a private broker locally")
		}
		return nil
	}
	expected := 1
	if *b.DeploymentMode == standby {
		expected = 2
	}
	if *b.DeploymentMode == cluster {
		expected = 3
	} // finite local restriction; AWS allows fewer
	if len(b.SubnetIds) != expected || len(b.SecurityGroups) < 1 || len(b.SecurityGroups) > 5 {
		return invalid(
			"Private brokers require explicit subnets (1 single, 2 standby, 3 cluster) and 1-5 security groups",
		)
	}
	if s.network == nil {
		return invalid("Scoped EC2 metadata lookup is unavailable")
	}
	vpc := ""
	seen := map[string]bool{}
	zones := map[string]bool{}
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
		az := common.Value(sn.AvailabilityZone)
		if common.Value(sn.VpcId) != vpc || az == "" || zones[az] {
			return invalid("Subnets must use one VPC and distinct Availability Zones")
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
		return invalid("SecurityGroups must reference existing scoped EC2 groups in the subnet VPC")
	}
	return nil
}
