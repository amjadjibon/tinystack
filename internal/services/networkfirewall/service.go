// Package networkfirewall implements a records-only AWS Network Firewall control plane.
// Resources (Firewalls, FirewallPolicies, RuleGroups, LoggingConfigurations, ResourcePolicies)
// are stored deterministically per account and Region.
// No network traffic is inspected and no actual VPC endpoints or routing modifications exist.
package networkfirewall

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName        = "networkfirewall"
	kindFirewall       = "firewall"
	kindPolicy         = "policy"
	kindRuleGroup      = "rulegroup"
	kindLogging        = "logging"
	kindResourcePolicy = "resourcepolicy"

	ruleGroupTypeStateless = "STATELESS"
	ruleGroupTypeStateful  = "STATEFUL"

	statusReady    = "READY"
	statusDeleting = "DELETING"
	syncInSync     = "IN_SYNC"
)

var _ api.API = (*Service)(nil)

// Service implements the Network Firewall control plane. It reads subnets through network, the EC2 service.
type Service struct {
	store   storage.Store
	network *ec2.Service
}

func New(store storage.Store, network *ec2.Service) *Service {
	return &Service{store: store, network: network}
}

// firewallRecord stores firewall state and attachments.
type firewallRecord struct {
	Firewall       *api.Firewall       `json:"Firewall"`
	FirewallStatus *api.FirewallStatus `json:"FirewallStatus"`
	SubnetAZs      map[string]string   `json:"SubnetAZs"`
	UpdateToken    string              `json:"UpdateToken"`
}

// policyRecord stores policy state and association tracking.
type policyRecord struct {
	PolicyResponse       *api.FirewallPolicyResponse `json:"PolicyResponse"`
	Policy               *api.FirewallPolicy         `json:"Policy"`
	UpdateToken          string                      `json:"UpdateToken"`
	NumberOfAssociations int64                       `json:"NumberOfAssociations"`
}

// ruleGroupRecord stores rule group state and association tracking.
type ruleGroupRecord struct {
	RuleGroupResponse    *api.RuleGroupResponse `json:"RuleGroupResponse"`
	RuleGroup            *api.RuleGroup         `json:"RuleGroup"`
	RulesString          *string                `json:"RulesString,omitempty"`
	UpdateToken          string                 `json:"UpdateToken"`
	NumberOfAssociations int64                  `json:"NumberOfAssociations"`
}

// loggingRecord stores firewall logging configuration.
type loggingRecord struct {
	LoggingConfiguration *api.LoggingConfiguration `json:"LoggingConfiguration"`
	FirewallArn          string                    `json:"FirewallArn"`
}

// resourcePolicyRecord stores an IAM-style resource policy attached to a policy or rule group.
type resourcePolicyRecord struct {
	ResourceArn string `json:"ResourceArn"`
	Policy      string `json:"Policy"`
}

func newUUID() string {
	return uuid.NewV4().String()
}

func keyExists(tx storage.ReadTx, key string) bool {
	_, ok := tx.Get(key)
	return ok
}

func firewallARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:network-firewall:%s:%s:firewall/%s", scope.Region, scope.AccountID, name)
}

func firewallPolicyARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:network-firewall:%s:%s:firewall-policy/%s", scope.Region, scope.AccountID, name)
}

func ruleGroupARN(scope request.Scope, ruleType, name string) string {
	prefix := "stateless-rulegroup"
	if strings.EqualFold(ruleType, ruleGroupTypeStateful) {
		prefix = "stateful-rulegroup"
	}

	return fmt.Sprintf("arn:aws:network-firewall:%s:%s:%s/%s", scope.Region, scope.AccountID, prefix, name)
}

func deterministicEndpointID(firewallName, subnetID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(firewallName))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(subnetID))
	return fmt.Sprintf("vpce-%017x", h.Sum64())
}

func parseNameFromARN(scope request.Scope, arn, resourceType string) (string, error) {
	expectedPrefix := fmt.Sprintf("arn:aws:network-firewall:%s:%s:%s/", scope.Region, scope.AccountID, resourceType)
	if !strings.HasPrefix(arn, expectedPrefix) {
		return "", awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found with ARN %s", arn))
	}

	name := strings.TrimPrefix(arn, expectedPrefix)
	if name == "" {
		return "", awserror.New("InvalidRequestException", "Invalid resource ARN format")
	}

	return name, nil
}

func resolveFirewallName(scope request.Scope, name, arn *string) (string, error) {
	if name != nil && *name != "" {
		return *name, nil
	}

	if arn != nil && *arn != "" {
		return parseNameFromARN(scope, *arn, kindFirewall)
	}

	return "", awserror.New("InvalidRequestException", "Either FirewallName or FirewallArn must be specified")
}

func resolvePolicyName(scope request.Scope, name, arn *string) (string, error) {
	if name != nil && *name != "" {
		return *name, nil
	}

	if arn != nil && *arn != "" {
		return parseNameFromARN(scope, *arn, "firewall-policy")
	}

	return "", awserror.New(
		"InvalidRequestException",
		"Either FirewallPolicyName or FirewallPolicyArn must be specified",
	)
}

func resolveRuleGroupName(scope request.Scope, name, arn *string) (string, error) {
	if name != nil && *name != "" {
		return *name, nil
	}

	if arn != nil && *arn != "" {
		// Rule groups can be stateless-rulegroup or stateful-rulegroup
		if n, err := parseNameFromARN(scope, *arn, "stateless-rulegroup"); err == nil {
			return n, nil
		}

		if n, err := parseNameFromARN(scope, *arn, "stateful-rulegroup"); err == nil {
			return n, nil
		}

		return "", awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found with ARN %s", *arn))
	}

	return "", awserror.New("InvalidRequestException", "Either RuleGroupName or RuleGroupArn must be specified")
}

func checkUpdateToken(provided *string, expected string) error {
	if provided == nil || *provided == "" {
		return awserror.New("InvalidTokenException", "UpdateToken is required")
	}

	if *provided != expected {
		return awserror.New("InvalidTokenException", "UpdateToken does not match current resource state")
	}

	return nil
}

func now() *float64 {
	return new(float64(time.Now().Unix()))
}
