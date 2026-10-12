package transfer

import (
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

var (
	serverPattern = regexp.MustCompile(`^s-[0-9a-f]{17}$`)
	keyPattern    = regexp.MustCompile(`^key-[0-9a-f]{17}$`)
	userPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_@.-]{2,99}$`)
	rolePattern   = regexp.MustCompile(
		`^arn:aws:iam::([0-9]{12}):role/([\x21-\x7e]+/)?([A-Za-z0-9_+=,.@-]{1,64})$`,
	)
	arnPattern = regexp.MustCompile(
		`^arn:aws:transfer:([a-z0-9-]+):([0-9]{12}):(server|user)/(s-[0-9a-f]{17})(?:/([A-Za-z0-9_][A-Za-z0-9_@.-]{2,99}))?$`,
	)
)

func ValidateServerConfiguration(s *api.DescribedServer) error {
	if common.Value(s.Domain) != "S3" || common.Value(s.EndpointType) != "PUBLIC" ||
		common.Value(s.IdentityProviderType) != serviceManaged ||
		len(s.Protocols) != 1 ||
		s.Protocols[0] != "SFTP" {
		return invalid(
			"Only explicit Protocols=[SFTP], EndpointType=PUBLIC, Domain=S3 and " +
				"IdentityProviderType=SERVICE_MANAGED records are supported",
		)
	}
	if common.Value(s.SecurityPolicyName) != "TransferSecurityPolicy-2018-11" &&
		common.Value(s.SecurityPolicyName) != "TransferSecurityPolicy-2024-01" {
		return invalid(
			"SecurityPolicyName must explicitly select TransferSecurityPolicy-2018-11 or TransferSecurityPolicy-2024-01",
		)
	}
	banner := common.Value(s.PreAuthenticationLoginBanner)
	if len(banner) > 4096 {
		return invalid("PreAuthenticationLoginBanner exceeds 4096 bytes")
	}
	for _, r := range banner {
		if (r < 9 || r > 13) && (r < 32 || r > 126) {
			return invalid("PreAuthenticationLoginBanner requires ASCII printable text or tab/newline controls")
		}
	}
	return checkTags(s.Tags)
}

func ValidateUserConfiguration(u *api.DescribedUser, scope request.Scope) error {
	if !userPattern.MatchString(common.Value(u.UserName)) {
		return invalid("Invalid UserName: requires 3-100 ASCII username characters")
	}
	role := common.Value(u.Role)
	p := rolePattern.FindStringSubmatch(role)
	if len(p) != 4 || p[1] != scope.AccountID || len(role) > 2048 || len(p[2])+1 > 512 {
		return invalid("Role requires a same-account commercial IAM role ARN; existence and trust are not evaluated")
	}
	if common.Value(u.HomeDirectoryType) != "PATH" {
		return invalid("Only HomeDirectoryType=PATH is supported")
	}
	// An explicit absolute path avoids inventing a default directory or verified bucket reference.
	path := common.Value(u.HomeDirectory)
	if path == "" || !strings.HasPrefix(path, "/") || !utf8.ValidString(path) || utf8.RuneCountInString(path) > 1024 ||
		strings.ContainsAny(path, "\r\n\x00") {
		return invalid("HomeDirectory requires an explicit absolute path of at most 1024 characters locally")
	}
	return checkTags(u.Tags)
}

func checkTags(tags []api.Tag) error {
	if len(tags) > 50 {
		return invalid("A resource can have at most 50 tags")
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if t.Key == nil || t.Value == nil {
			return invalid("Tag Key and Value are required")
		}
		k, v := *t.Key, *t.Value
		if !utf8.ValidString(k) || !utf8.ValidString(v) || utf8.RuneCountInString(k) > 128 ||
			utf8.RuneCountInString(v) > 256 ||
			strings.HasPrefix(strings.ToLower(k), "aws:") {
			return invalid("Tags require keys up to 128 and values up to 256 characters; aws: keys are reserved")
		}
		if seen[k] {
			return invalid("Duplicate tag keys")
		}
		seen[k] = true
	}
	return nil
}
