package mediaconnect

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CloudFormationUpdate applies complete template properties, including optional-property removal.
// It preserves resource identifiers and uses the same reference/state checks as API updates.
func (s *Service) CloudFormationUpdate(ctx context.Context, flowARN, kind, id string, input any) error {
	switch in := input.(type) {
	case *api.CreateFlowRequest:
		if checkErr := in.Validate(""); checkErr != nil {
			return invalid("%v", checkErr)
		}

		if in.Source == nil {
			return invalid("Source is required")
		}

		if checkErr := s.references(
			ctx,
			[]api.SetSourceRequest{*in.Source},
			nil,
			nil,
			in.VpcInterfaces,
		); checkErr != nil {
			return checkErr
		}

	case *api.SetSourceRequest:
		if checkErr := in.Validate(""); checkErr != nil {
			return invalid("%v", checkErr)
		}

		if checkErr := s.encryption(ctx, in.Decryption); checkErr != nil {
			return checkErr
		}

	case *api.AddOutputRequest:
		if checkErr := in.Validate(""); checkErr != nil {
			return invalid("%v", checkErr)
		}

		if checkErr := s.encryption(ctx, in.Encryption); checkErr != nil {
			return checkErr
		}

	case *api.GrantEntitlementRequest:
		if checkErr := in.Validate(""); checkErr != nil {
			return invalid("%v", checkErr)
		}

		if checkErr := s.encryption(ctx, in.Encryption); checkErr != nil {
			return checkErr
		}

	case *api.VpcInterfaceRequest:
		if checkErr := in.Validate(""); checkErr != nil {
			return invalid("%v", checkErr)
		}

		if checkErr := s.requireRole(ctx, in.RoleArn); checkErr != nil {
			return checkErr
		}

	default:
		return invalid("Unsupported template input")
	}

	_, err := s.mutate(ctx, new(flowARN), func(tx storage.WriteTx, scope request.Scope, r *record) error {
		switch in := input.(type) {
		case *api.CreateFlowRequest:
			if in.Source == nil {
				return invalid("Source is required")
			}

			if len(r.Flow.Sources) == 0 {
				return missing()
			}

			old := r.Flow.Sources[0]
			source := convertSource(in.Source)
			source.SourceArn, source.Name = old.SourceArn, common.OrElse(in.Source.Name, old.Name)
			r.Flow.Sources[0] = source
			r.Flow.SourceFailoverConfig = common.Clone(in.SourceFailoverConfig)
			r.Flow.VpcInterfaces = slices.DeleteFunc(r.Flow.VpcInterfaces, func(v api.VpcInterface) bool {
				return slices.Contains(r.InlineVpcs, common.Value(v.Name))
			})
			r.InlineVpcs = nil
			for _, v := range in.VpcInterfaces {
				r.InlineVpcs = append(r.InlineVpcs, common.Value(v.Name))
			}

			if checkErr := s.addVpcs(tx, scope, r, in.VpcInterfaces); checkErr != nil {
				return checkErr
			}

		case *api.SetSourceRequest:
			found := false
			for i, v := range r.Flow.Sources {
				if common.Value(v.SourceArn) == id {
					value := convertSource(in)
					value.SourceArn, value.Name = v.SourceArn, v.Name
					r.Flow.Sources[i] = value
					found = true
				}
			}

			if !found {
				return missing()
			}

		case *api.AddOutputRequest:
			found := false
			for i := range r.Flow.Outputs {
				v := &r.Flow.Outputs[i]
				if common.Value(v.OutputArn) != id {
					continue
				}

				value := convertOutput(in)
				value.OutputArn, value.Name = v.OutputArn, v.Name
				value.OutputStatus = common.OrElse(in.OutputStatus, new("ENABLED"))
				r.Flow.Outputs[i] = value
				found = true
			}

			if !found {
				return missing()
			}

		case *api.GrantEntitlementRequest:
			if len(in.Subscribers) == 0 {
				return invalid("Subscribers are required")
			}

			found := false
			for i, v := range r.Flow.Entitlements {
				if common.Value(v.EntitlementArn) != id {
					continue
				}

				value := convertEntitlement(in)
				value.EntitlementArn, value.Name = v.EntitlementArn, v.Name
				value.EntitlementStatus = common.OrElse(in.EntitlementStatus, new("ENABLED"))
				r.Flow.Entitlements[i] = value
				found = true
			}

			if !found {
				return missing()
			}

		case *api.VpcInterfaceRequest:
			found := false
			for i, v := range r.Flow.VpcInterfaces {
				if common.Value(v.Name) == id {
					r.Flow.VpcInterfaces = append(r.Flow.VpcInterfaces[:i], r.Flow.VpcInterfaces[i+1:]...)
					found = true
					break
				}
			}

			if !found {
				return missing()
			}

			if checkErr := s.addVpcs(tx, scope, r, []api.VpcInterfaceRequest{*in}); checkErr != nil {
				return checkErr
			}
		}

		return validateFlow(tx, scope, r)
	})
	return err
}
