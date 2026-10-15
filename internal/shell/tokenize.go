// Package shell parses and runs the command lines of the console's CloudShell:
// "aws <service> <operation> [--flags]", mapped onto the generated catalog. It
// has no host shell, pipes, redirects or variables; the gateway runs each
// command in-process with the signed-in person's identity.
package shell

import (
	"errors"
	"strings"
)

// syntaxHint ends the messages about shell syntax the CloudShell does not run.
const syntaxHint = "use the aws-local wrapper (cmd/aws-local) with a real shell for that"

const variableMessage = "variables and command substitution are not supported; " + syntaxHint +
	" (single quotes keep $ and ` literal)"

// Token is one word of a command line.
type Token struct {
	Text string
	// Quoted words are values even when they start with "--".
	Quoted bool
}

// Tokenize splits a command line into words the way a POSIX shell does for
// quoting: single quotes are literal, double quotes allow backslash escapes,
// and a backslash escapes the next character. Pipes, redirects, command
// separators, variables and command substitution are rejected.
func Tokenize(line string) ([]Token, error) {
	var (
		tokens  []Token
		word    strings.Builder
		started bool
		quoted  bool
	)

	flush := func() {
		if started {
			tokens = append(tokens, Token{Text: word.String(), Quoted: quoted})
		}

		word.Reset()
		started, quoted = false, false
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		case r == '\'':
			started, quoted = true, true
			end := i + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}

			if end == len(runes) {
				return nil, errors.New("unterminated single quote")
			}

			word.WriteString(string(runes[i+1 : end]))
			i = end
		case r == '"':
			started, quoted = true, true
			closed := false
			for i++; i < len(runes); i++ {
				c := runes[i]
				if c == '"' {
					closed = true
					break
				}

				if c == '\\' && i+1 < len(runes) && strings.ContainsRune("\"\\$`\n", runes[i+1]) {
					i++
					if runes[i] != '\n' {
						word.WriteRune(runes[i])
					}

					continue
				}

				if c == '`' || (c == '$' && i+1 < len(runes) && substitutes(runes[i+1])) {
					return nil, errors.New(variableMessage)
				}

				word.WriteRune(c)
			}

			if !closed {
				return nil, errors.New("unterminated double quote")
			}
		case r == '\\':
			if i+1 == len(runes) {
				return nil, errors.New("a line cannot end with a backslash")
			}

			i++
			started = true
			if runes[i] != '\n' {
				word.WriteRune(runes[i])
			}
		case strings.ContainsRune("|&;<>()`", r):
			return nil, errors.New("pipes, redirects, command lists and subshells (" + string(r) +
				") are not supported; " + syntaxHint)
		case r == '$':
			return nil, errors.New(variableMessage)
		default:
			started = true
			word.WriteRune(r)
		}
	}

	flush()

	return tokens, nil
}

// substitutes reports whether a "$" followed by r starts an expansion.
func substitutes(r rune) bool {
	return r == '(' || r == '{' || r == '_' || r == '?' || r == '@' || r == '*' || r == '#' || r == '!' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
