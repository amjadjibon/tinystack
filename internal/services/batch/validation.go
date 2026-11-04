package batch

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxInteger int64 = 2147483647

var (
	rolePattern     = regexp.MustCompile(`^[A-Za-z0-9_+=,.@/-]{1,512}$`)
	instancePattern = regexp.MustCompile(
		`^(c5|m5|r5)(\.(large|xlarge|2xlarge|4xlarge|8xlarge|12xlarge|16xlarge|24xlarge|metal))?$`,
	)
	subnetPattern = regexp.MustCompile(`^subnet-[0-9a-f]{8,40}$`)
	groupPattern  = regexp.MustCompile(`^sg-[0-9a-f]{8,40}$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func stateOK(p *string) error {
	if p != nil && *p != enabled && *p != disabled {
		return invalid("State must be ENABLED or DISABLED")
	}

	return nil
}

func integerOK(p *int64, minimum int64, field string) error {
	if p == nil || *p < minimum || *p > maxInteger {
		return invalid(field + " is required and must be an integer in the supported range")
	}

	return nil
}

func capacityOK(c *api.ComputeResource) error {
	if guardErr := integerOK(c.MinvCpus, 0, "minvCpus"); guardErr != nil {
		return guardErr
	}

	if guardErr := integerOK(c.MaxvCpus, 1, "maxvCpus"); guardErr != nil {
		return guardErr
	}

	if guardErr := integerOK(c.DesiredvCpus, 0, "desiredvCpus"); guardErr != nil {
		return guardErr
	}

	if *c.MinvCpus > *c.DesiredvCpus || *c.DesiredvCpus > *c.MaxvCpus {
		return invalid("Require minvCpus <= desiredvCpus <= maxvCpus")
	}

	return nil
}

// IAM metadata accepts scoped ARNs or documented short names where allowed;
// existence, trust and permissions are never evaluated.
func roleOK(scope request.Scope, p *string, kind string, short bool) error {
	if p == nil {
		return nil
	}

	v := *p
	prefix := "arn:aws:iam::" + scope.AccountID + ":" + kind + "/"
	if strings.HasPrefix(v, "arn:") {
		if !strings.HasPrefix(v, prefix) {
			return invalid("IAM ARN must have the correct kind and caller account")
		}

		v = strings.TrimPrefix(v, prefix)
	} else {
		if !short {
			return invalid("A full scoped IAM role ARN is required")
		}

		v = strings.TrimPrefix(v, "/")
	}

	if !rolePattern.MatchString(v) || strings.Contains(v, "//") || strings.HasSuffix(v, "/") ||
		strings.HasPrefix(v, "/") {
		return invalid("Invalid IAM role/profile identifier")
	}

	return nil
}

func linkedRole(p *string) bool {
	return strings.Contains(common.Value(p), "role/aws-service-role/batch.amazonaws.com/AWSServiceRoleForBatch")
}

func (s *Service) computeOK(tx storage.ReadTx, scope request.Scope, c *api.ComputeResource) error {
	if c == nil {
		return invalid("MANAGED environments require computeResources")
	}

	fargate := isFargate(common.Value(c.Type))
	if !fargate && common.Value(c.Type) != ec2Platform {
		return invalid("Compute type must be EC2, FARGATE or FARGATE_SPOT")
	}

	if fargate {
		if c.MinvCpus != nil || c.DesiredvCpus != nil || c.AllocationStrategy != nil ||
			c.InstanceTypes != nil || c.InstanceRole != nil {
			return invalid("Fargate does not support EC2 capacity, instance or allocation fields")
		}

		if err := integerOK(c.MaxvCpus, 1, "maxvCpus"); err != nil {
			return err
		}
	} else {
		if guardErr := capacityOK(c); guardErr != nil {
			return guardErr
		}

		if a := common.Value(c.AllocationStrategy); a != "BEST_FIT" && a != "BEST_FIT_PROGRESSIVE" {
			return invalid("Only BEST_FIT and BEST_FIT_PROGRESSIVE are supported")
		}

		if len(c.InstanceTypes) < 1 || len(c.InstanceTypes) > 100 {
			return invalid("instanceTypes requires 1-100 unique supported x86 choices")
		}

		seen := map[string]bool{}
		for _, v := range c.InstanceTypes {
			if seen[v] || (!instancePattern.MatchString(v) && v != "optimal" && v != "default_x86_64") {
				return invalid("Supported x86 instance choices: c5/m5/r5 families and sizes, optimal, default_x86_64")
			}

			seen[v] = true
		}

		if c.InstanceRole == nil {
			return invalid("EC2 instanceRole is required")
		}

		if guardErr := roleOK(scope, c.InstanceRole, "instance-profile", true); guardErr != nil {
			return guardErr
		}
	}

	if len(c.Subnets) < 1 || len(c.Subnets) > 16 || len(c.SecurityGroupIds) < 1 || len(c.SecurityGroupIds) > 5 {
		return invalid("Require 1-16 subnets and 1-5 security groups")
	}

	if s.network == nil {
		return invalid("EC2 network metadata is unavailable")
	}

	vpc := ""
	seen := map[string]bool{}
	for _, id := range c.Subnets {
		if !subnetPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate subnet ID")
		}

		seen[id] = true
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return invalid("Subnet must exist in the caller account and Region")
		}

		if vpc != "" && vpc != common.Value(sn.VpcId) {
			return invalid("Subnets must belong to one VPC")
		}

		vpc = common.Value(sn.VpcId)
	}

	seen = map[string]bool{}
	for _, id := range c.SecurityGroupIds {
		if !groupPattern.MatchString(id) || seen[id] {
			return invalid("Invalid or duplicate security group ID")
		}

		seen[id] = true
	}

	if _, err := s.network.MountGroups(tx, scope, vpc, c.SecurityGroupIds); err != nil {
		return invalid("Security groups must exist in the subnet VPC and caller scope")
	}

	return nil
}

func tagsOK(tags map[string]string) error {
	if guardErr := tagging.CheckSet(tags); guardErr != nil {
		return invalid(guardErr.Error())
	}

	return nil
}

func stringsUnique(list []string, maximum int) error {
	if len(list) > maximum {
		return invalid("Too many references")
	}

	seen := map[string]bool{}
	for _, v := range list {
		if seen[v] {
			return invalid("Duplicate reference")
		}

		seen[v] = true
	}

	return nil
}

func containerOK(scope request.Scope, c *api.ContainerProperties, platform string) error {
	if c == nil || c.Image == nil || len(*c.Image) < 1 || len(*c.Image) > 255 ||
		strings.ContainsAny(*c.Image, " \t\r\n") {
		return invalid("containerProperties.image requires 1-255 non-whitespace characters")
	}

	if guardErr := roleOK(scope, c.JobRoleArn, "role", false); guardErr != nil {
		return guardErr
	}

	if guardErr := roleOK(scope, c.ExecutionRoleArn, "role", false); guardErr != nil {
		return guardErr
	}

	fargate := platform == fargatePlatform
	if err := platformOK(c, fargate); err != nil {
		return err
	}

	cpu, mem := c.Vcpus, c.Memory
	if len(c.ResourceRequirements) > 0 {
		if cpu != nil || mem != nil {
			return invalid("Use resourceRequirements or vcpus/memory, not both")
		}

		seen := map[string]bool{}
		for _, r := range c.ResourceRequirements {
			t := common.Value(r.Type)
			if seen[t] || (t != resourceVCPU && t != resourceMemory) {
				return invalid("Only one VCPU and one MEMORY requirement are supported")
			}

			seen[t] = true
			if fargate && t == resourceVCPU {
				continue
			}

			n, err := strconv.ParseInt(common.Value(r.Value), 10, 32)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != common.Value(r.Value) {
				return invalid("Resource requirement values must be positive decimal integers")
			}

			if t == resourceVCPU {
				cpu = new(n)
			} else {
				mem = new(n)
			}
		}
	}

	if !fargate {
		if guardErr := integerOK(cpu, 1, "vcpus/VCPU"); guardErr != nil {
			return guardErr
		}

		if guardErr := integerOK(mem, 4, "memory/MEMORY"); guardErr != nil {
			return guardErr
		}
	}

	if len(c.Environment) > 100 || len(c.Command) > 100 {
		return invalid("At most 100 environment or command entries")
	}

	seen := map[string]bool{}
	for _, v := range c.Environment {
		name := common.Value(v.Name)
		if !envPattern.MatchString(name) || len(name) > 256 || strings.HasPrefix(name, "AWS_BATCH") || seen[name] ||
			v.Value == nil ||
			len(*v.Value) > 4096 {
			return invalid("Invalid, reserved or duplicate environment variable")
		}

		seen[name] = true
	}

	for _, v := range c.Command {
		if len(v) > 4096 || strings.ContainsRune(v, 0) {
			return invalid("Invalid command entry")
		}
	}

	if l := c.LogConfiguration; l != nil {
		if common.Value(l.LogDriver) != "awslogs" {
			return invalid("Only awslogs is supported")
		}

		for k, v := range l.Options {
			if k != "awslogs-group" && k != "awslogs-region" && k != "awslogs-stream-prefix" {
				return invalid("Unsupported awslogs option: " + k)
			}

			if k == "awslogs-region" && v != scope.Region {
				return invalid("Cross-region logging is unsupported")
			}
		}
	}

	return nil
}

func parametersOK(params map[string]string) error {
	if len(params) > 100 {
		return invalid("At most 100 parameters")
	}

	for k, v := range params {
		if utf8.RuneCountInString(k) < 1 || len(k) > 256 || len(v) > 4096 {
			return invalid("Parameter keys require 1-256 bytes and values at most 4096 bytes")
		}
	}

	return nil
}

// These documented constraints are absent from the pinned Smithy model.
func executionOptionsOK(strategy *api.RetryStrategy, timeout *api.JobTimeout) error {
	if timeout != nil && (timeout.AttemptDurationSeconds == nil || *timeout.AttemptDurationSeconds < 60) {
		return invalid("attemptDurationSeconds must be at least 60")
	}

	if strategy == nil {
		return nil
	}

	if strategy.Attempts != nil && (*strategy.Attempts < 1 || *strategy.Attempts > 10) {
		return invalid("attempts must be 1-10")
	}

	if len(strategy.EvaluateOnExit) > 0 && strategy.Attempts == nil {
		return invalid("attempts is required with evaluateOnExit")
	}

	if len(strategy.EvaluateOnExit) > 6 {
		return invalid("At most six evaluateOnExit rules are supported")
	}

	for _, rule := range strategy.EvaluateOnExit {
		if rule.OnExitCode == nil && rule.OnReason == nil && rule.OnStatusReason == nil {
			return invalid("evaluateOnExit requires a condition")
		}

		for _, pattern := range []*string{rule.OnExitCode, rule.OnReason, rule.OnStatusReason} {
			if pattern != nil &&
				(len(*pattern) < 1 || len(*pattern) > 512 || strings.Contains(strings.TrimSuffix(*pattern, "*"), "*")) {
				return invalid("Retry patterns require 1-512 characters and an optional trailing wildcard")
			}
		}
	}

	return nil
}
