package ssm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	sweepInterval = time.Second

	policyExpiration         = "Expiration"
	policyExpirationNotice   = "ExpirationNotification"
	policyNoChangeNotice     = "NoChangeNotification"
	policyStatusPending      = "Pending"
	maxPolicyAttribute       = 1000000
	expirationIndexPrefix    = "aws/ssm/index/param/"
	policyVersion            = "1.0"
	expirationTimestampLabel = "Timestamp"
)

// policy is one parameter policy of an advanced parameter: its type and its
// JSON text, as DescribeParameters returns them. TinyStack enforces
// Expiration; the notification policies are stored and reported only, since
// nothing receives their EventBridge events.
type policy struct {
	Type string
	Text string
}

// policyOutput describes the policies of v. They stay Pending: an expired
// parameter is deleted.
func (v *version) policyOutput() []api.ParameterInlinePolicy {
	var out []api.ParameterInlinePolicy
	for _, p := range v.Policies {
		out = append(out, api.ParameterInlinePolicy{
			PolicyText: new(p.Text), PolicyType: new(p.Type), PolicyStatus: new(policyStatusPending),
		})
	}

	return out
}

func invalidAttribute(format string, args ...any) error {
	return awserror.New("InvalidPolicyAttributeException", fmt.Sprintf(format, args...))
}

// parsePolicies validates the JSON array of policies that PutParameter
// takes, and returns them with the time the Expiration policy deletes the
// parameter, in Unix milliseconds, or 0.
func parsePolicies(text string, now time.Time) (policies []policy, expires int64, err error) {
	var elements []struct {
		Type       *string
		Version    *string
		Attributes map[string]any
	}

	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&elements); decodeErr != nil {
		return nil, 0, invalidAttribute("The policies are not a valid JSON array of policy objects with a Type, " +
			"a Version, and Attributes.")
	}

	seen := map[string]bool{}
	for _, e := range elements {
		kind := value(e.Type, "")
		switch kind {
		case policyExpiration, policyExpirationNotice, policyNoChangeNotice:
		default:
			return nil, 0, awserror.New("InvalidPolicyTypeException",
				"The policy type is not supported: "+kind+". Use Expiration, ExpirationNotification, or "+
					"NoChangeNotification.")
		}

		if seen[kind] {
			return nil, 0, awserror.New("IncompatiblePolicyException", "A parameter can have only one "+kind+
				" policy.")
		}

		seen[kind] = true
		if e.Version != nil && *e.Version != policyVersion {
			return nil, 0, invalidAttribute("The %s policy version must be %s.", kind, policyVersion)
		}

		at, attributeErr := checkPolicyAttributes(kind, e.Attributes, now)
		if attributeErr != nil {
			return nil, 0, attributeErr
		}

		if kind == policyExpiration {
			expires = at.UnixMilli()
		}

		body, _ := json.Marshal(map[string]any{keyType: kind, "Version": policyVersion, "Attributes": e.Attributes})
		policies = append(policies, policy{Type: kind, Text: string(bytes.TrimSpace(body))})
	}

	if seen[policyExpirationNotice] && !seen[policyExpiration] {
		return nil, 0, awserror.New("IncompatiblePolicyException",
			"An ExpirationNotification policy requires an Expiration policy.")
	}

	return policies, expires, nil
}

// checkPolicyAttributes validates the attributes of a policy of kind. It
// returns the expiration time for an Expiration policy.
func checkPolicyAttributes(kind string, attributes map[string]any, now time.Time) (time.Time, error) {
	allowed := map[string][]string{
		policyExpiration:       {expirationTimestampLabel},
		policyExpirationNotice: {"Before", "Unit"},
		policyNoChangeNotice:   {"After", "Unit"},
	}[kind]
	for name := range attributes {
		if !contains(allowed, name) {
			return time.Time{}, invalidAttribute("The %s policy has no attribute %s.", kind, name)
		}
	}

	if kind == policyExpiration {
		raw, _ := attributes[expirationTimestampLabel].(string)
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, invalidAttribute("The Expiration policy needs a Timestamp in ISO 8601 UTC format, " +
				"such as 2030-12-02T21:34:33.000Z.")
		}

		if !at.After(now) {
			return time.Time{}, invalidAttribute("The Expiration policy Timestamp must be in the future.")
		}

		return at, nil
	}

	amount := "Before"
	if kind == policyNoChangeNotice {
		amount = "After"
	}

	number, ok := wholeNumber(attributes[amount])
	if !ok || number < 1 || number > maxPolicyAttribute {
		return time.Time{}, invalidAttribute("The %s policy needs %s, a whole number of 1 or more.", kind, amount)
	}

	if unit, _ := attributes["Unit"].(string); unit != "Days" && unit != "Hours" {
		return time.Time{}, invalidAttribute("The %s policy Unit must be Days or Hours.", kind)
	}

	return time.Time{}, nil
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if s == item {
			return true
		}
	}

	return false
}

// wholeNumber reads a policy attribute that is a JSON number or a numeric
// string, as AWS documents the examples both ways.
func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), n == float64(int64(n))
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		return parsed, err == nil
	}

	return 0, false
}

func expirationKey(scope request.Scope, name string) string {
	return expirationIndexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

// sweep applies the parameter policies that are due and finishes commands.
// It runs every sweepInterval and does each step in its own transaction.
func (s *Service) sweep(ctx context.Context) {
	if err := s.expire(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("ssm: expiring parameters failed", "error", err)
	}

	if err := s.finishCommands(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("ssm: finishing commands failed", "error", err)
	}
}

// expire deletes the parameters whose Expiration policy is due.
func (s *Service) expire(ctx context.Context) error {
	now := s.now().UnixMilli()

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(expirationIndexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, expirationIndexPrefix), "/", 3)
			if len(parts) != 3 {
				tx.Delete(k)
				continue
			}

			scope := request.Scope{AccountID: parts[0], Region: parts[1]}
			p, found, err := storage.Read[parameter](tx, parameterKey(scope, parts[2]))
			if err != nil {
				return err
			}

			var v *version
			if found {
				if v, _, err = loadVersion(tx, scope, p.Name, p.Version); err != nil {
					return err
				}
			}

			switch {
			case v == nil || v.ExpiresAt == 0:
				tx.Delete(k)
			case v.ExpiresAt <= now:
				deleteParameter(tx, scope, p.Name)
			}
		}

		return nil
	})
}
