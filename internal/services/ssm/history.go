package ssm

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxHistory = 50

// checkBareName validates a parameter name that may not carry a selector.
func checkBareName(name *string) error {
	if err := checkName(name); err != nil {
		return err
	}

	if strings.Contains(*name, ":") {
		return validation("Parameter name must not include a version or label selector.")
	}

	return nil
}

// GetParameterHistory lists every version of a parameter, oldest first.
func (s *Service) GetParameterHistory(
	ctx context.Context,
	in *api.GetParameterHistoryRequest,
) (*api.GetParameterHistoryResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkBareName(in.Name); guardErr != nil {
		return nil, guardErr
	}

	decrypt := in.WithDecryption != nil && *in.WithDecryption
	out := &api.GetParameterHistoryResult{Parameters: []api.ParameterHistory{}}
	var decrypted []*version
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadParameter(tx, m.Scope, *in.Name)
		if loadErr != nil {
			return loadErr
		}

		var versions []*version
		for _, k := range tx.Keys(versionPrefix(m.Scope, p.Name)) {
			v, _, readErr := storage.Read[version](tx, k)
			if readErr != nil {
				return readErr
			}

			versions = append(versions, v)
		}

		// Version numbers are zero-padded so that they sort like the keys.
		number := func(v *version) string { return fmt.Sprintf("%010d", v.Version) }
		selected, next, pageErr := page(versions, number, in.NextToken, in.MaxResults, maxHistory)
		if pageErr != nil {
			return pageErr
		}

		labels := labelsOf(p)
		decrypted = selected
		for _, v := range selected {
			out.Parameters = append(out.Parameters, api.ParameterHistory{
				Name:             new(p.Name),
				Type:             new(v.Type),
				KeyId:            optional(v.KeyID),
				LastModifiedDate: seconds(v.ModifiedAt),
				Description:      optional(v.Description),
				Value:            new(v.value(decrypt)),
				AllowedPattern:   optional(v.AllowedPattern),
				Version:          new(v.Version),
				Labels:           labels[v.Version],
				Tier:             new(v.Tier),
				DataType:         new(v.DataType),
			})
		}

		out.NextToken = next
		return nil
	})
	if err != nil {
		return nil, err
	}

	if decrypt {
		if keyErr := s.decrypting(ctx, m.Scope, decrypted); keyErr != nil {
			return nil, keyErr
		}
	}

	return out, nil
}

// checkLabels requires 1 to 10 labels.
func checkLabels(labels []string) error {
	if len(labels) == 0 || len(labels) > maxLabels {
		return validation("Labels must list 1 to %d labels.", maxLabels)
	}

	return nil
}

// updateVersion runs fn on a parameter and one of its versions, the current
// one when number is nil, and saves the parameter.
func (s *Service) updateVersion(
	ctx context.Context,
	name *string,
	number *int64,
	fn func(p *parameter, v int64),
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	if guardErr := checkBareName(name); guardErr != nil {
		return guardErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadParameter(tx, m.Scope, *name)
		if loadErr != nil {
			return loadErr
		}

		v := p.Version
		if number != nil {
			v = *number
		}

		if _, ok := tx.Get(versionKey(m.Scope, p.Name, v)); !ok {
			return versionNotFound(fmt.Sprint(v), p.Name)
		}

		fn(p, v)
		if len(labelsOf(p)[v]) > maxLabels {
			return awserror.New("ParameterVersionLabelLimitExceeded", fmt.Sprintf("A parameter version can "+
				"have maximum %d labels. Move one or more labels to another version and try again.", maxLabels))
		}

		return storage.Write(tx, parameterKey(m.Scope, p.Name), p)
	})
}

// LabelParameterVersion puts labels on a version, moving any that another
// version has. Invalid labels are returned, not applied.
func (s *Service) LabelParameterVersion(
	ctx context.Context,
	in *api.LabelParameterVersionRequest,
) (*api.LabelParameterVersionResult, error) {
	if err := checkLabels(in.Labels); err != nil {
		return nil, err
	}

	out := &api.LabelParameterVersionResult{InvalidLabels: []string{}}
	err := s.updateVersion(ctx, in.Name, in.ParameterVersion, func(p *parameter, v int64) {
		for _, label := range in.Labels {
			if !checkLabel(label) {
				out.InvalidLabels = append(out.InvalidLabels, label)
				continue
			}

			if p.Labels == nil {
				p.Labels = map[string]int64{}
			}

			p.Labels[label] = v
		}

		out.ParameterVersion = new(v)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UnlabelParameterVersion removes labels from a version. Labels the version
// does not have are returned as invalid.
func (s *Service) UnlabelParameterVersion(
	ctx context.Context,
	in *api.UnlabelParameterVersionRequest,
) (*api.UnlabelParameterVersionResult, error) {
	if err := checkLabels(in.Labels); err != nil {
		return nil, err
	}

	if in.ParameterVersion == nil {
		return nil, validation("ParameterVersion is required.")
	}

	out := &api.UnlabelParameterVersionResult{RemovedLabels: []string{}, InvalidLabels: []string{}}
	err := s.updateVersion(ctx, in.Name, in.ParameterVersion, func(p *parameter, v int64) {
		for _, label := range in.Labels {
			if labeled, ok := p.Labels[label]; !ok || labeled != v {
				out.InvalidLabels = append(out.InvalidLabels, label)
				continue
			}

			delete(p.Labels, label)
			out.RemovedLabels = append(out.RemovedLabels, label)
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
