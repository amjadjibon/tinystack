//go:build console

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// shellStrict reports whether CloudShell limits commands to shellCommands. With
// OIDC, people other than the host's owner sign in, and the AWS CLI can read and
// write host files and start host programs. Without OIDC the console user is
// the local user, who could run the same CLI in a terminal, so every command runs.
func (s *Server) shellStrict() bool { return s.cfg.Console.Auth.OIDC != nil }

// Only reviewed, ordinary API commands belong here. Do not derive this from the
// installed CLI or the generated catalog: customizations can replace modeled
// commands (e.g. lambda invoke and s3api get-object write positional files).
// Adding a command requires checking the AWS CLI's customizations and streaming
// input/output handling, not just the corresponding AWS API.
var shellCommands = map[string][]string{
	"sts": {"get-caller-identity"},
	"s3api": {
		"list-buckets",
		"create-bucket",
		"delete-bucket",
		"head-bucket",
		"list-objects-v2",
		"head-object",
		"delete-object",
	},
	"sqs": {
		"list-queues", "create-queue", "delete-queue", "get-queue-url", "get-queue-attributes", "set-queue-attributes",
		"send-message", "receive-message", "delete-message", "purge-queue",
	},
	"dynamodb": {
		"list-tables", "describe-table", "create-table", "delete-table", "get-item", "put-item", "update-item",
		"delete-item", "query", "scan", "batch-get-item", "batch-write-item", "execute-statement",
	},
	"iam":            {"list-users", "list-roles", "list-policies", "get-user", "get-role", "get-policy"},
	"lambda":         {"list-functions", "get-function-configuration"},
	"cloudformation": {"list-stacks", "describe-stacks", "list-stack-resources", "describe-stack-events"},
	"logs":           {"describe-log-groups", "describe-log-streams", "get-log-events", "filter-log-events"},
	"secretsmanager": {"list-secrets", "describe-secret", "get-secret-value", "create-secret", "put-secret-value"},
	"ssm":            {"get-parameter", "get-parameters", "put-parameter", "delete-parameter", "describe-parameters"},
}

func shellPolicy(args []string) error {
	if len(args) < 2 || !slices.Contains(shellCommands[args[0]], args[1]) {
		return errors.New("CloudShell permits only reviewed API commands; see the CloudShell guide")
	}

	for _, arg := range args[2:] {
		// Paramfile substitution also occurs inside shorthand values (name@=...).
		// URL loading can read internal services, including metadata credentials.
		for _, scheme := range []string{"file://", "fileb://", "http://", "https://"} {
			if strings.Contains(strings.ToLower(arg), scheme) {
				return errors.New("CloudShell does not load files or URLs; supply inline values")
			}
		}

		if arg == "help" || arg == "-h" || strings.ContainsRune(arg, 0) {
			return errors.New("CloudShell does not support CLI help or NUL bytes")
		}

		name, _, _ := strings.Cut(arg, "=")
		// argparse accepts unique abbreviations, including options introduced by
		// CLI v2. Reject every prefix of globals that alter execution or read files.
		for _, option := range []string{
			"--endpoint-url", "--profile", "--cli-auto-prompt", "--debug", "--ca-bundle", "--no-sign-request",
			"--help", "--cli-input-json", "--cli-input-yaml", "--generate-cli-skeleton", "--no-verify-ssl",
		} {
			if strings.HasPrefix(name, "--") && strings.HasPrefix(option, name) {
				return errors.New("CloudShell does not permit this CLI option")
			}
		}
	}

	return nil
}

// completeShellStatic completes from shellCommands under OIDC. It never passes
// partial input to aws_completer, which would load CLI customizations on the host.
func completeShellStatic(w http.ResponseWriter, line string) {
	candidates := []string{}
	parts := strings.Split(line, " ")
	if len(parts) == 2 && parts[0] == shellBinary {
		for service := range shellCommands {
			if strings.HasPrefix(service, parts[1]) {
				candidates = append(candidates, service)
			}
		}
	} else if len(parts) == 3 && parts[0] == shellBinary {
		for _, command := range shellCommands[parts[1]] {
			if strings.HasPrefix(command, parts[2]) {
				candidates = append(candidates, command)
			}
		}
	}

	slices.Sort(candidates)
	_ = json.NewEncoder(w).Encode(shellCandidates{Candidates: candidates})
}
