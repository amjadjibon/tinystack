package localtls

import (
	"fmt"
	"net"
	"strings"
)

// suffixes are the endpoint domains the gateway answers TLS for: AWS service,
// S3, API Gateway, Lambda URL and Cognito hosted domains, LocalStack's names,
// and localhost subdomains.
var suffixes = []string{
	".amazonaws.com", ".amazonaws.com.cn", ".api.aws", ".on.aws", ".amazoncognito.com",
	".localhost.localstack.cloud", ".localhost",
}

// ValidateNames checks extra names for the certificates: DNS names, wildcards
// of one leading label, or IP literals.
func ValidateNames(names []string) error {
	for _, name := range names {
		if net.ParseIP(name) == nil && !validName(name) {
			return fmt.Errorf("tls.names: invalid DNS name or IP %q", name)
		}
	}

	return nil
}

// covered reports whether name, lower-cased, is a gateway endpoint name or
// matches one of extra.
func covered(name string, extra []string) bool {
	if name == "localhost" || name == "localhost.localstack.cloud" {
		return true
	}

	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	for _, pattern := range extra {
		pattern = strings.ToLower(pattern)
		if wildcard, ok := strings.CutPrefix(pattern, "*."); ok {
			label, rest, found := strings.Cut(name, ".")
			if found && label != "" && rest == wildcard {
				return true
			}
		} else if name == pattern {
			return true
		}
	}

	return false
}

func validName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}

	name = strings.TrimPrefix(name, "*.")
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}

	return true
}
