package iam

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	simulationAllowed  = "allowed"
	simulationImplicit = "implicitDeny"
	simulationExplicit = "explicitDeny"
)

func (s *Service) SimulateCustomPolicy(ctx context.Context,
	in *api.SimulateCustomPolicyRequest,
) (*api.SimulatePolicyResponse, error) {
	if len(in.PolicyInputList) == 0 {
		return nil, invalidInput("PolicyInputList is required")
	}

	return simulate(ctx, in, &Authorization{})
}

func (s *Service) SimulatePrincipalPolicy(ctx context.Context,
	in *api.SimulatePrincipalPolicyRequest,
) (*api.SimulatePolicyResponse, error) {
	meta, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	principal, err := simulationPrincipal(common.Value(in.PolicySourceArn))
	if err != nil {
		return nil, err
	}

	if principal.Account != meta.AccountID {
		return nil, invalidInput("PolicySourceArn must belong to the caller's account")
	}

	snapshot, err := s.simulationAuthorization(ctx, &principal)
	if err != nil {
		return nil, err
	}

	caller := in.CallerArn
	if caller == nil {
		caller = in.PolicySourceArn
	}

	return simulate(ctx, &api.SimulateCustomPolicyRequest{
		PolicyInputList: in.PolicyInputList, PermissionsBoundaryPolicyInputList: in.PermissionsBoundaryPolicyInputList,
		ActionNames: in.ActionNames, ResourceArns: in.ResourceArns, ResourcePolicy: in.ResourcePolicy,
		ResourceOwner: in.ResourceOwner, CallerArn: caller, ContextEntries: in.ContextEntries,
		MaxItems: in.MaxItems, Marker: in.Marker,
	}, &snapshot)
}

func simulationPrincipal(value string) (request.Principal, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != awsPartition || parts[2] != "iam" ||
		parts[3] != "" || !accountID.MatchString(parts[4]) {
		return request.Principal{}, invalidInput("Expected an IAM user, group or role ARN")
	}

	kind, path, ok := strings.Cut(parts[5], "/")
	if !ok || (kind != kindUser && kind != kindGroup && kind != kindRole) || path == "" ||
		strings.ContainsAny(path, "*?") || strings.HasSuffix(path, "/") {
		return request.Principal{}, invalidInput("Expected an IAM user, group or role ARN")
	}

	names := strings.Split(path, "/")
	return request.Principal{
		Kind: kind, Name: names[len(names)-1], Account: parts[4], ARN: value, PolicyARN: value,
	}, nil
}

// Resolve the ARN exactly, then use the gateway's shared policy collector. Stable
// IDs make a concurrent deletion/recreation fail closed instead of changing identity.
func (s *Service) simulationAuthorization(ctx context.Context, principal *request.Principal) (Authorization, error) {
	out := Authorization{Principal: *principal}
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		switch principal.Kind {
		case kindUser:
			u, ok, err := storage.Read[user](tx, key(principal.Account, kindUser, principal.Name))
			if err != nil {
				return err
			}

			if ok && userARN(principal.Account, u) == principal.ARN {
				principal.ID, found = u.ID, true
			}
		case kindRole:
			r, ok, err := storage.Read[role](tx, key(principal.Account, kindRole, principal.Name))
			if err != nil {
				return err
			}

			if ok && roleARN(principal.Account, r) == principal.ARN {
				principal.ID, found = r.ID, true
			}
		case kindGroup:
			g, ok, err := storage.Read[group](tx, key(principal.Account, kindGroup, principal.Name))
			if err != nil {
				return err
			}

			if ok && groupARN(principal.Account, g) == principal.ARN {
				found = true
				return collectPolicies(tx, principal.Account, principal.ARN, &g.permissions, &out)
			}
		}

		return nil
	})
	if err != nil {
		return out, awserror.New("PolicyEvaluationException", err.Error())
	}

	if !found {
		return out, noSuchEntity("Policy source %s cannot be found", principal.ARN)
	}

	if principal.Kind != kindGroup {
		snapshot, err := s.Authorization(ctx, principal)
		if err != nil {
			return out, awserror.New("PolicyEvaluationException", err.Error())
		}

		return snapshot, nil
	}

	return out, nil
}

func simulationDocuments(input []string, prefix, sourceType string) []iampolicy.Document {
	out := make([]iampolicy.Document, len(input))
	for i, document := range input {
		out[i] = iampolicy.Document{Name: fmt.Sprintf("%s.%d", prefix, i+1), JSON: document, SourceType: sourceType}
	}

	return out
}

func simulationContext(entries []api.ContextEntry) (map[string][]string, error) {
	values := map[string][]string{}
	for _, entry := range entries {
		name := strings.ToLower(common.Value(entry.ContextKeyName))
		kind := common.Value(entry.ContextKeyType)
		base := strings.TrimSuffix(kind, "List")
		if name == "" || len(entry.ContextKeyValues) == 0 ||
			(len(entry.ContextKeyValues) != 1 && kind == base) {
			return nil, invalidInput("Context entries require a name, type and values")
		}

		if _, duplicate := values[name]; duplicate {
			return nil, invalidInput("Duplicate context key %s", name)
		}

		for _, value := range entry.ContextKeyValues {
			valid := false
			switch base {
			case "string":
				valid = true
			case "numeric":
				number, err := strconv.ParseFloat(value, 64)
				valid = err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
			case "boolean":
				valid = value == "true" || value == "false"
			case "ip":
				_, err := netip.ParseAddr(value)
				valid = err == nil
			case "binary":
				_, err := base64.StdEncoding.DecodeString(value)
				valid = err == nil
			case "date":
				_, err := time.Parse(time.RFC3339Nano, value)
				valid = err == nil
			}

			if !valid {
				return nil, invalidInput("Invalid context type or value for %s", name)
			}
		}

		values[name] = slices.Clone(entry.ContextKeyValues)
	}

	return values, nil
}

func simulate(ctx context.Context, in *api.SimulateCustomPolicyRequest,
	snapshot *Authorization,
) (*api.SimulatePolicyResponse, error) {
	meta, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.ActionNames) == 0 || len(in.PermissionsBoundaryPolicyInputList) > 1 {
		return nil, invalidInput("ActionNames is required and at most one permissions boundary is supported")
	}

	for _, action := range in.ActionNames {
		service, name, ok := strings.Cut(action, ":")
		if !ok || service == "" || name == "" || strings.ContainsAny(action, "*?") {
			return nil, invalidInput("ActionNames must contain service:action names without wildcards")
		}
	}

	if len(snapshot.Unsupported) > 0 {
		return nil, invalidInput("%s", strings.Join(snapshot.Unsupported, "; "))
	}

	documents := slices.Clone(snapshot.Documents)
	documents = append(documents, simulationDocuments(in.PolicyInputList, "PolicyInputList", "user")...)
	boundary := snapshot.Boundary
	if len(in.PermissionsBoundaryPolicyInputList) > 0 {
		boundary = simulationDocuments(
			in.PermissionsBoundaryPolicyInputList,
			"PermissionsBoundaryPolicyInputList",
			"user",
		)
	}

	for _, doc := range append(slices.Clone(documents), boundary...) {
		if len(doc.JSON) > maxDocument {
			return nil, invalidInput("Policy document exceeds maximum length")
		}

		if validationErr := iampolicy.ValidateSimulation(doc.JSON, false); validationErr != nil {
			return nil, invalidInput("%s: %s", doc.Name, validationErr)
		}
	}

	values, err := simulationContext(in.ContextEntries)
	if err != nil {
		return nil, err
	}

	principal := request.Principal{Account: meta.AccountID}
	if in.CallerArn != nil {
		principal, err = simulationPrincipal(*in.CallerArn)
		if err != nil {
			return nil, err
		}
	}

	if in.ResourcePolicy != nil {
		if in.CallerArn == nil || principal.Kind != kindUser || snapshot.Principal.Kind == kindRole {
			return nil, invalidInput(
				"ResourcePolicy requires an IAM user CallerArn; role/group resource simulation is unsupported",
			)
		}

		if len(*in.ResourcePolicy) > maxDocument {
			return nil, invalidInput("Resource policy exceeds maximum length")
		}

		if validationErr := iampolicy.ValidateSimulation(*in.ResourcePolicy, true); validationErr != nil {
			return nil, invalidInput("ResourcePolicy: %s", validationErr)
		}
	}

	owner := principal.Account
	if in.ResourceOwner != nil {
		owner = *in.ResourceOwner
		if !accountID.MatchString(owner) {
			return nil, invalidInput("ResourceOwner must be an AWS account ID")
		}
	}

	resources := in.ResourceArns
	if len(resources) == 0 {
		resources = []string{"*"}
	}

	// Like AWS, each action and resource pair is its own result.
	results := make([]api.EvaluationResult, 0, len(in.ActionNames)*len(resources))
	for _, action := range in.ActionNames {
		for _, resource := range resources {
			resourceOwner := owner
			parts := strings.SplitN(resource, ":", 6)
			if resource != "*" && (len(parts) != 6 || parts[0] != "arn") {
				return nil, invalidInput("Invalid resource ARN %s", resource)
			}

			if in.ResourceOwner == nil && len(parts) == 6 && parts[4] != "" {
				resourceOwner = parts[4]
			}

			evaluated, evaluationErr := simulateResource(documents, boundary, in, action, resource,
				iampolicy.Context{Values: values, Simulation: true, ResourceAccount: resourceOwner}, &principal)
			if evaluationErr != nil {
				return nil, evaluationErr
			}

			result := api.EvaluationResult{
				EvalActionName: new(action), EvalResourceName: new(resource),
				EvalDecision:                      evaluated.EvalResourceDecision,
				MatchedStatements:                 evaluated.MatchedStatements,
				MissingContextValues:              evaluated.MissingContextValues,
				PermissionsBoundaryDecisionDetail: evaluated.PermissionsBoundaryDecisionDetail,
			}
			if len(in.ResourceArns) > 0 {
				result.ResourceSpecificResults = []api.ResourceSpecificResult{evaluated}
			}

			results = append(results, result)
		}
	}

	limit := int64(defaultPageSize)
	if in.MaxItems != nil {
		limit = *in.MaxItems
	}

	if limit < 1 || limit > maxPageSize {
		return nil, invalidInput("MaxItems must be between 1 and 1000")
	}

	// Index keys preserve caller action order and permit repeated action names.
	indices := make([]string, len(results))
	for i := range indices {
		indices[i] = fmt.Sprintf("%020d", i)
	}

	selected, marker, err := common.Page(indices, func(v string) string { return v }, in.Marker,
		int(limit), invalidInput("Invalid Marker"))
	if err != nil {
		return nil, err
	}

	out := &api.SimulatePolicyResponse{IsTruncated: new(marker != nil), Marker: marker}
	for _, index := range selected {
		i, _ := strconv.Atoi(index)
		out.EvaluationResults = append(out.EvaluationResults, results[i])
	}

	return out, nil
}

func simulateResource(documents, boundary []iampolicy.Document, in *api.SimulateCustomPolicyRequest,
	action, resource string, evalContext iampolicy.Context, principal *request.Principal,
) (api.ResourceSpecificResult, error) {
	identity := iampolicy.Evaluate(documents, action, resource, evalContext)
	decisions := []iampolicy.Decision{identity}
	allowed, denied := identity.Allowed, identity.ExplicitDeny
	limitsAllow := true
	out := api.ResourceSpecificResult{EvalResourceName: new(resource)}
	if len(boundary) > 0 {
		limit := iampolicy.Evaluate(boundary, action, resource, evalContext)
		limitsAllow = limit.Allowed
		allowed, denied = allowed && limit.Allowed, denied || limit.ExplicitDeny
		decisions = append(decisions, limit)
		out.PermissionsBoundaryDecisionDetail = &api.PermissionsBoundaryDecisionDetail{
			AllowedByPermissionsBoundary: new(limit.Allowed),
		}
	}

	if in.ResourcePolicy != nil {
		resourceDecision := iampolicy.EvaluateResource(iampolicy.Document{
			Name: "ResourcePolicy", JSON: *in.ResourcePolicy, SourceType: "resource",
		}, action, resource, evalContext, principal)
		if evalContext.ResourceAccount != principal.Account {
			allowed = allowed && resourceDecision.Allowed
		} else {
			allowed = allowed || (resourceDecision.Allowed && (resourceDecision.BypassesLimits || limitsAllow))
		}

		denied = denied || resourceDecision.ExplicitDeny
		decisions = append(decisions, resourceDecision)
	}

	decision := simulationImplicit
	if denied {
		decision = simulationExplicit
	} else if allowed {
		decision = simulationAllowed
	}

	out.EvalResourceDecision = new(decision)
	for _, evaluated := range decisions {
		if len(evaluated.Unsupported) > 0 {
			return out, invalidInput("%s", strings.Join(evaluated.Unsupported, "; "))
		}

		out.MissingContextValues = append(out.MissingContextValues, evaluated.Simulation.Missing...)
		for _, match := range evaluated.Simulation.Matches {
			if (denied && match.Effect != "Deny") || (!denied && match.Effect != "Allow") ||
				decision == simulationImplicit {
				continue
			}

			sourceType := match.PolicyType
			if sourceType == "" {
				sourceType = principal.Kind
			}

			out.MatchedStatements = append(out.MatchedStatements, api.Statement{
				SourcePolicyId: new(match.PolicyID), SourcePolicyType: new(sourceType),
			})
		}
	}

	slices.Sort(out.MissingContextValues)
	out.MissingContextValues = slices.Compact(out.MissingContextValues)
	out.MatchedStatements = uniqueStatements(out.MatchedStatements)
	return out, nil
}

func uniqueStatements(statements []api.Statement) []api.Statement {
	out := make([]api.Statement, 0, len(statements))
	for _, statement := range statements {
		if !slices.ContainsFunc(out, func(other api.Statement) bool {
			return common.Value(other.SourcePolicyId) == common.Value(statement.SourcePolicyId) &&
				common.Value(other.SourcePolicyType) == common.Value(statement.SourcePolicyType)
		}) {
			out = append(out, statement)
		}
	}

	return out
}
