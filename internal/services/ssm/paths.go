package ssm

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxByPath   = 10
	maxDescribe = 50

	optionEquals     = "Equals"
	optionBeginsWith = "BeginsWith"
	optionContains   = "Contains"
	optionOneLevel   = "OneLevel"
	optionRecursive  = "Recursive"
)

// current is a parameter with its current version.
type current struct {
	*parameter
	*version
}

func (c current) name() string { return c.Name }

// filter reports whether a parameter matches one ParameterStringFilter.
type filter func(c current) bool

// under reports whether name is below path: directly, or at any depth when
// recursive.
func under(name, path string, recursive bool) bool {
	prefix := strings.TrimSuffix(path, "/") + "/"
	rest, ok := strings.CutPrefix(name, prefix)
	return ok && rest != "" && (recursive || !strings.Contains(rest, "/"))
}

func checkPath(path *string) error {
	if path == nil || !strings.HasPrefix(*path, "/") || len(*path) > 2048 || !nameChars.MatchString(*path) ||
		strings.Count(strings.TrimSuffix(*path, "/"), "/") > maxLevels {
		return validation("The parameter path must begin with a forward slash (/) and contain only letters, " +
			"numbers, and the symbols .-_/, in at most 15 levels.")
	}

	return nil
}

// parseFilters compiles ParameterStringFilters. DescribeParameters also
// accepts the Name, Path, and tag filters.
func parseFilters(filters []api.ParameterStringFilter, describe bool) ([]filter, error) {
	out := make([]filter, 0, len(filters))
	for _, f := range filters {
		key, option := value(f.Key, ""), value(f.Option, optionEquals)
		values := f.Values
		if len(values) == 0 {
			return nil, awserror.New("InvalidFilterValue", "The filter "+key+" needs at least one value.")
		}

		var field func(c current) string
		options := []string{optionEquals}
		switch {
		case key == keyType:
			field = func(c current) string { return c.Type }
		case key == "KeyId":
			field = func(c current) string { return c.KeyID }
		case key == "Tier":
			field = func(c current) string { return c.Tier }
		case key == "DataType":
			field = func(c current) string { return c.DataType }
		case describe && key == "Name":
			field, options = current.name, []string{optionEquals, optionBeginsWith, optionContains}
		case describe && strings.HasPrefix(key, "tag:") && len(key) > len("tag:"):
			tag := key[len("tag:"):]
			field = func(c current) string { return c.Tags[tag] }
		case describe && key == "Path":
			if option == optionEquals && f.Option == nil {
				option = optionOneLevel
			}

			if option != optionOneLevel && option != optionRecursive {
				return nil, awserror.New("InvalidFilterOption", "The Path filter supports OneLevel and Recursive.")
			}

			for i := range values {
				if err := checkPath(&values[i]); err != nil {
					return nil, awserror.New("InvalidFilterValue", "The Path filter value is not a valid path.")
				}
			}

			out = append(out, func(c current) bool {
				return slices.ContainsFunc(values, func(p string) bool {
					return under(c.Name, p, option == optionRecursive)
				})
			})

			continue
		default:
			return nil, awserror.New("InvalidFilterKey", "The following filter key is not valid: "+key)
		}

		if !slices.Contains(options, option) {
			return nil, awserror.New("InvalidFilterOption",
				"The filter option is not valid: "+option+". Valid options include: "+strings.Join(options, ", "))
		}

		out = append(out, func(c current) bool {
			got := field(c)
			return slices.ContainsFunc(values, func(want string) bool {
				switch option {
				case optionBeginsWith:
					return strings.HasPrefix(got, want)
				case optionContains:
					return strings.Contains(got, want)
				}

				return got == want
			})
		})
	}

	return out, nil
}

// matching returns every parameter in scope, ordered by name, with its
// current version, that keep accepts and every filter matches.
func matching(tx storage.ReadTx, scope request.Scope, keep func(name string) bool, filters []filter) (
	[]current,
	error,
) {
	var out []current
	prefix := parameterKey(scope, "")
	for _, k := range tx.Keys(prefix) {
		if !keep(k[len(prefix):]) {
			continue
		}

		p, _, err := storage.Read[parameter](tx, k)
		if err != nil {
			return nil, err
		}

		v, _, err := loadVersion(tx, scope, p.Name, p.Version)
		if err != nil {
			return nil, err
		}

		c := current{parameter: p, version: v}
		if !slices.ContainsFunc(filters, func(f filter) bool { return !f(c) }) {
			out = append(out, c)
		}
	}

	return out, nil
}

// GetParametersByPath returns the parameters below a path, one level deep or
// recursively.
func (s *Service) GetParametersByPath(
	ctx context.Context,
	in *api.GetParametersByPathRequest,
) (*api.GetParametersByPathResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkPath(in.Path); guardErr != nil {
		return nil, guardErr
	}

	filters, err := parseFilters(in.ParameterFilters, false)
	if err != nil {
		return nil, err
	}

	recursive := in.Recursive != nil && *in.Recursive
	keep := func(name string) bool { return under(name, *in.Path, recursive) }
	decrypt := in.WithDecryption != nil && *in.WithDecryption
	out := &api.GetParametersByPathResult{Parameters: []api.Parameter{}}
	var secrets []revealed
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, listErr := matching(tx, m.Scope, keep, filters)
		if listErr != nil {
			return listErr
		}

		for _, c := range s.publicParameters(m.Region) {
			if keep(c.Name) && !slices.ContainsFunc(filters, func(f filter) bool { return !f(c) }) {
				found = append(found, c)
			}
		}

		slices.SortFunc(found, func(a, b current) int { return strings.Compare(a.Name, b.Name) })

		selected, next, pageErr := page(found, current.name, in.NextToken, in.MaxResults, maxByPath)
		if pageErr != nil {
			return pageErr
		}

		for _, c := range selected {
			out.Parameters = append(out.Parameters, c.version.parameter(m.Scope, c.Name, ""))
			if decrypt && c.Type == typeSecureString {
				i := len(out.Parameters) - 1
				secrets = append(secrets, revealed{
					version: c.version, name: c.Name, set: func(plain string) { out.Parameters[i].Value = &plain },
				})
			}
		}

		out.NextToken = next
		return nil
	})
	if err != nil {
		return nil, err
	}

	if keyErr := s.reveal(ctx, m.Scope, secrets); keyErr != nil {
		return nil, keyErr
	}

	return out, nil
}

// DescribeParameters lists parameter metadata, never values.
func (s *Service) DescribeParameters(
	ctx context.Context,
	in *api.DescribeParametersRequest,
) (*api.DescribeParametersResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	filters, err := parseFilters(in.ParameterFilters, true)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeParametersResult{Parameters: []api.ParameterMetadata{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, listErr := matching(tx, m.Scope, func(string) bool { return true }, filters)
		if listErr != nil {
			return listErr
		}

		selected, next, pageErr := page(found, current.name, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		for _, c := range selected {
			out.Parameters = append(out.Parameters, c.metadata(m.Scope, c.Name))
		}

		out.NextToken = next
		return nil
	})

	return out, err
}
