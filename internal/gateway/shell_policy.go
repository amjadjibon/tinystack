//go:build console

package gateway

import (
	"errors"
	"path"
	"strings"
)

// shellAllowed reports whether console.cloudshell.allow permits command of
// service, or any of its commands when command is empty. An empty list allows
// everything. Patterns were validated when the configuration loaded.
func (s *Server) shellAllowed(service, command string) bool {
	allow := s.cfg.Console.CloudShell.Allow
	if len(allow) == 0 {
		return true
	}

	for _, pattern := range allow {
		servicePattern, commandPattern, _ := strings.Cut(pattern, ":")
		if ok, _ := path.Match(servicePattern, service); !ok {
			continue
		}

		if ok, _ := path.Match(commandPattern, command); ok || command == "" {
			return true
		}
	}

	return false
}

// shellPolicy applies console.cloudshell.allow to a command's argv (without aws).
// The service and command must come first so that global options and their
// values are never mistaken for them.
func (s *Server) shellPolicy(args []string) error {
	if len(s.cfg.Console.CloudShell.Allow) == 0 {
		return nil
	}

	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New("CloudShell is limited to console.cloudshell.allow: write aws <service> <command> " +
			"and put global options after the command")
	}

	if !s.shellAllowed(args[0], args[1]) {
		return errors.New("console.cloudshell.allow does not permit " + args[0] + " " + args[1])
	}

	return nil
}

// shellAllowedCandidates drops completions for services and commands the
// allowlist rejects. Later words complete only within a permitted command.
func (s *Server) shellAllowedCandidates(line string, candidates []string) []string {
	if len(s.cfg.Console.CloudShell.Allow) == 0 {
		return candidates
	}

	words := strings.Fields(line)
	if strings.HasSuffix(line, " ") {
		words = append(words, "")
	}

	// words[0] is aws; words[len-1] is the word being completed.
	position := len(words) - 1
	if position > 2 {
		if s.shellPolicy(words[1:3]) != nil {
			return []string{}
		}

		return candidates
	}

	kept := []string{}
	for _, candidate := range candidates {
		if position == 1 && s.shellAllowed(candidate, "") ||
			position == 2 && candidate != "" && s.shellAllowed(words[1], candidate) {
			kept = append(kept, candidate)
		}
	}

	return kept
}
