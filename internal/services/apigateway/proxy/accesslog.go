package proxy

import (
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// contextPattern finds $context variables in an access log format.
var contextPattern = regexp.MustCompile(`\$context\.[A-Za-z0-9_.]+`)

// FormatLog renders an access log format, replacing each $context variable
// with lookup's value for its name (such as "requestId"), or "-" when lookup
// has none, as API Gateway does.
func FormatLog(format string, lookup func(name string) string) string {
	return contextPattern.ReplaceAllStringFunc(format, func(v string) string {
		if got := lookup(strings.TrimPrefix(v, "$context.")); got != "" {
			return got
		}

		return "-"
	})
}

// LogGroup returns the account, region, and name of the CloudWatch Logs log
// group an ARN names.
func LogGroup(arn string) (request.Scope, string, bool) {
	parts := strings.SplitN(arn, ":", 7)
	if len(parts) != 7 || parts[5] != "log-group" {
		return request.Scope{}, "", false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, strings.TrimSuffix(parts[6], ":*"), true
}
