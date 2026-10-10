package config

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

var (
	resourceName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	roleName     = regexp.MustCompile(`^([A-Za-z0-9_+=,.@-]+/)*[A-Za-z0-9_+=,.@-]{1,64}$`)
	bucketName   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	// These identifiers and resource types are metadata; none executes an evaluator.
	managedRules = []string{
		"REQUIRED_TAGS",
		"S3_BUCKET_PUBLIC_READ_PROHIBITED",
		"S3_BUCKET_PUBLIC_WRITE_PROHIBITED",
		"EC2_INSTANCE_NO_PUBLIC_IP",
	}
	resourceTypes = []string{"AWS::S3::Bucket", "AWS::EC2::Instance", "AWS::IAM::Role", "AWS::Lambda::Function"}
	frequencies   = []string{"One_Hour", "Three_Hours", "Six_Hours", "Twelve_Hours", "TwentyFour_Hours"}
)

func validName(name string, limit int) bool {
	return len(name) <= limit && resourceName.MatchString(name)
}

func length(p *string, minimum, maximum int) bool {
	if p == nil {
		return true
	}
	n := utf8.RuneCountInString(*p)
	return n >= minimum && n <= maximum
}

func checkTypes(types []string) error {
	seen := map[string]bool{}
	for _, t := range types {
		if !slices.Contains(resourceTypes, t) || seen[t] {
			return invalid("Resource types must be unique supported types: " + strings.Join(resourceTypes, ", "))
		}
		seen[t] = true
	}
	return nil
}

func checkRule(rule *api.ConfigRule) error {
	if rule == nil || rule.Source == nil {
		return invalid("ConfigRule.Source is required")
	}
	if common.Value(rule.Source.Owner) != "AWS" {
		return invalid(
			"Only AWS managed rule source metadata is supported; CUSTOM_LAMBDA and CUSTOM_POLICY are unsupported",
		)
	}
	if !slices.Contains(managedRules, common.Value(rule.Source.SourceIdentifier)) {
		return invalid("Supported SourceIdentifier values: " + strings.Join(managedRules, ", "))
	}
	if !length(rule.Description, 0, 256) || !length(rule.InputParameters, 1, 1024) {
		return invalid("Description or InputParameters length is invalid")
	}
	if rule.InputParameters != nil {
		var params map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*rule.InputParameters), &params); err != nil || params == nil {
			return invalid("InputParameters must be a JSON object")
		}
	}
	if rule.ConfigRuleState != nil {
		return invalid("ConfigRuleState is read-only; evaluation lifecycle changes are unsupported")
	}
	if sc := rule.Scope; sc != nil {
		if len(sc.ComplianceResourceTypes) > 100 {
			return invalid("Scope has too many resource types")
		}
		if err := checkTypes(sc.ComplianceResourceTypes); err != nil {
			return err
		}
		if !length(sc.ComplianceResourceId, 1, 768) || !length(sc.TagKey, 1, 128) || !length(sc.TagValue, 1, 256) {
			return invalid("Scope member length is invalid")
		}
		if sc.ComplianceResourceId != nil && len(sc.ComplianceResourceTypes) != 1 {
			return invalid("ComplianceResourceId requires exactly one resource type")
		}
		if sc.TagValue != nil && sc.TagKey == nil {
			return invalid("TagValue requires TagKey")
		}
		if sc.TagKey != nil && (len(sc.ComplianceResourceTypes) > 0 || sc.ComplianceResourceId != nil) {
			return invalid("Tag scope cannot be combined with resource type or ID scope")
		}
	}
	return nil
}

func checkGroup(group *api.RecordingGroup) error {
	if group == nil {
		return nil
	}
	all := common.Value(group.AllSupported)
	if all && len(group.ResourceTypes) > 0 || !all && len(group.ResourceTypes) == 0 ||
		!all && common.Value(group.IncludeGlobalResourceTypes) {
		return failure(
			"InvalidRecordingGroupException",
			"Choose allSupported or explicit resourceTypes; global types require allSupported",
		)
	}
	if err := checkTypes(group.ResourceTypes); err != nil {
		return failure("InvalidRecordingGroupException", err.Error())
	}
	return nil
}

func parseTags(tags []api.Tag) (map[string]string, error) {
	values, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, common.OrElse(t.Value, new("")) },
		invalid("Tags require Key"),
		func(string) error { return invalid("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}
	if err = tagging.CheckSet(values); err != nil {
		return nil, tagError(err)
	}
	return values, nil
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return failure("TooManyTagsException", err.Error())
	}
	return invalid(err.Error())
}
