package ssm

import (
	"context"
	"errors"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutParameter creates a parameter, or with Overwrite adds a version to one.
// Settings a new version leaves out keep their current values.
func (s *Service) PutParameter(ctx context.Context, in *api.PutParameterRequest) (*api.PutParameterResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName(in.Name); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkNewName(m.Scope, *in.Name); guardErr != nil {
		return nil, guardErr
	}

	if in.Value == nil || *in.Value == "" {
		return nil, validation("Parameter value must be at least 1 character long.")
	}

	if in.Description != nil && len(*in.Description) > maxDescription {
		return nil, validation("Description must be at most %d characters long.", maxDescription)
	}

	overwrite := in.Overwrite != nil && *in.Overwrite
	if overwrite && len(in.Tags) > 0 {
		return nil, validation("Invalid request: tags and overwrite can't be used together. To create a parameter " +
			"with tags, please remove overwrite flag. To update tags for an existing parameter, please use " +
			"AddTagsToResource or RemoveTagsFromResource.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	var policies *parsedPolicies
	if in.Policies != nil {
		parsed, expires, policyErr := parsePolicies(*in.Policies, s.now())
		if policyErr != nil {
			return nil, policyErr
		}

		policies = &parsedPolicies{list: parsed, expires: expires}
	}

	sealed, err := s.sealNew(ctx, m.Scope, in, overwrite)
	if err != nil {
		return nil, err
	}

	var out *api.PutParameterResult
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pk := parameterKey(m.Scope, *in.Name)
		p, exists, readErr := storage.Read[parameter](tx, pk)
		if readErr != nil {
			return readErr
		}

		previous := &version{}
		if exists {
			if !overwrite {
				return awserror.New("ParameterAlreadyExists", "The parameter already exists. To overwrite this "+
					"value, set the overwrite option in the request to true.")
			}

			if previous, _, readErr = loadVersion(tx, m.Scope, p.Name, p.Version); readErr != nil {
				return readErr
			}
		} else {
			p = &parameter{Name: *in.Name, Tags: tags}
		}

		next, settingsErr := settings(in, previous, sealed, policies)
		if settingsErr != nil {
			return settingsErr
		}

		next.Version, next.ModifiedAt = p.Version+1, now
		if pruneErr := makeRoom(tx, m.Scope, p); pruneErr != nil {
			return pruneErr
		}

		p.Version = next.Version
		if writeErr := storage.Write(tx, versionKey(m.Scope, p.Name, next.Version), next); writeErr != nil {
			return writeErr
		}

		if next.ExpiresAt > 0 {
			if writeErr := storage.Write(tx, expirationKey(m.Scope, p.Name), next.ExpiresAt); writeErr != nil {
				return writeErr
			}
		} else {
			tx.Delete(expirationKey(m.Scope, p.Name))
		}

		out = &api.PutParameterResult{Version: new(next.Version), Tier: new(next.Tier)}
		return storage.Write(tx, pk, p)
	})

	return out, err
}

// parsedPolicies are the policies a PutParameter request sets, and when the
// Expiration policy deletes the parameter.
type parsedPolicies struct {
	list    []policy
	expires int64
}

// sealed is the KMS encryption of a SecureString value, under keyID.
type sealed struct {
	keyID      string
	ciphertext string
}

// sealNew encrypts the value of a PutParameter request that creates a
// SecureString version, outside any transaction. It returns the zero value
// for the other types.
func (s *Service) sealNew(ctx context.Context, scope request.Scope, in *api.PutParameterRequest, overwrite bool) (
	sealed,
	error,
) {
	var previous *version
	if overwrite {
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			p, found, readErr := storage.Read[parameter](tx, parameterKey(scope, *in.Name))
			if readErr != nil || !found {
				return readErr
			}

			previous, _, readErr = loadVersion(tx, scope, p.Name, p.Version)

			return readErr
		})
		if err != nil {
			return sealed{}, err
		}
	}

	if previous == nil {
		previous = &version{}
	}

	if value(in.Type, previous.Type) != typeSecureString || in.Value == nil || *in.Value == "" {
		return sealed{}, nil
	}

	keyID := value(in.KeyId, value(&previous.KeyID, defaultKeyID))
	ciphertext, err := s.seal(ctx, scope, *in.Name, keyID, *in.Value)
	if err != nil {
		return sealed{}, err
	}

	return sealed{keyID: keyID, ciphertext: ciphertext}, nil
}

// settings returns the version in describes, keeping previous's settings
// for those it leaves out.
func settings(in *api.PutParameterRequest, previous *version, secret sealed, policies *parsedPolicies) (
	*version,
	error,
) {
	next := &version{
		Value:          *in.Value,
		Type:           previous.Type,
		KeyID:          previous.KeyID,
		Description:    previous.Description,
		AllowedPattern: previous.AllowedPattern,
		Tier:           previous.Tier,
		DataType:       previous.DataType,
		Policies:       previous.Policies,
		ExpiresAt:      previous.ExpiresAt,
	}
	next.Type = value(in.Type, next.Type)
	next.Description = value(in.Description, next.Description)
	next.AllowedPattern = value(in.AllowedPattern, next.AllowedPattern)
	next.DataType = value(in.DataType, value(&next.DataType, dataTypeText))

	switch next.Type {
	case "":
		return nil, validation("A parameter type is required when you create a parameter.")
	case typeString, typeStringList, typeSecureString:
	default:
		return nil, awserror.New("UnsupportedParameterType", "The parameter type is not supported: "+next.Type)
	}

	if next.DataType != dataTypeText {
		return nil, validation("TinyStack supports only the text data type.")
	}

	if err := checkKey(in.KeyId, next); err != nil {
		return nil, err
	}

	if err := checkPattern(next.AllowedPattern, next.Value); err != nil {
		return nil, err
	}

	if policies != nil {
		next.Policies, next.ExpiresAt = policies.list, policies.expires
	}

	if err := checkTier(in.Tier, previous.Tier, next); err != nil {
		return nil, err
	}

	if next.Type == typeSecureString {
		if secret.keyID != next.KeyID {
			return nil, awserror.New("TooManyUpdates", "The parameter changed while it was being encrypted. "+
				"Try again.")
		}

		next.Ciphertext, next.Value = secret.ciphertext, ""
	}

	return next, nil
}

func value(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}

	return *p
}

// checkKey sets v's KMS key. A SecureString value uses the requested key, the
// current version's, or the AWS managed key, as PutParameter has checked;
// other types have none.
func checkKey(keyID *string, v *version) error {
	if v.Type != typeSecureString {
		if keyID != nil {
			return validation("KeyId is required for SecureString type parameter only.")
		}

		v.KeyID = ""
		return nil
	}

	v.KeyID = value(keyID, value(&v.KeyID, defaultKeyID))
	return nil
}

// checkPattern requires the whole value to match the allowed pattern.
func checkPattern(pattern, v string) error {
	if pattern == "" {
		return nil
	}

	compiled, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if len(pattern) > maxPattern || err != nil {
		return awserror.New("InvalidAllowedPatternException", "The allowed pattern is not a valid regular expression.")
	}

	if !compiled.MatchString(v) {
		return awserror.New("ParameterPatternMismatchException",
			"Parameter value, cannot be validated against allowedPattern: "+pattern)
	}

	return nil
}

// checkTier sets v's tier from the requested one, which Intelligent-Tiering
// picks by value size. An advanced parameter cannot become standard.
func checkTier(requested *string, previous string, v *version) error {
	tier := value(requested, value(&previous, tierStandard))
	switch tier {
	case tierIntelligent:
		tier = tierStandard
		if len(v.Value) > maxStandardValue || previous == tierAdvanced || len(v.Policies) > 0 {
			tier = tierAdvanced
		}
	case tierStandard, tierAdvanced:
	default:
		return validation("Tier must be Standard, Advanced, or Intelligent-Tiering.")
	}

	if tier == tierStandard && len(v.Policies) > 0 {
		return validation("Parameter policies can be assigned only to advanced-tier parameters. Set the Tier to " +
			"Advanced or Intelligent-Tiering.")
	}

	if previous == tierAdvanced && tier == tierStandard {
		return validation("This parameter uses the advanced-parameter tier. You can't downgrade a parameter from " +
			"the advanced-parameter tier to the standard-parameter tier.")
	}

	if tier == tierStandard && len(v.Value) > maxStandardValue {
		return validation("Standard tier parameters support a maximum parameter value of %d characters. To "+
			"create a larger parameter value, upgrade the parameter to use the advanced-parameter tier.",
			maxStandardValue)
	}

	if len(v.Value) > maxAdvancedValue {
		return validation("Parameter values can be at most %d characters long.", maxAdvancedValue)
	}

	v.Tier = tier
	return nil
}

// makeRoom deletes p's oldest version when p has the most versions it may
// keep, unless that version has a label.
func makeRoom(tx storage.WriteTx, scope request.Scope, p *parameter) error {
	keys := tx.Keys(versionPrefix(scope, p.Name))
	if len(keys) < maxVersions {
		return nil
	}

	oldest, _, err := storage.Read[version](tx, keys[0])
	if err != nil {
		return err
	}

	for _, labeled := range p.Labels {
		if labeled == oldest.Version {
			return awserror.New("ParameterMaxVersionLimitExceeded", "You attempted to create a new version of "+
				p.Name+" by calling the PutParameter API with the overwrite flag. The oldest version can't be "+
				"deleted because it has a label associated with it. Move the label to another version of the "+
				"parameter, and try again.")
		}
	}

	tx.Delete(keys[0])
	return nil
}

// tagMap converts request tags, reporting SSM's errors.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		validation("Tags must have a Key and a Value."),
		nil,
	)
	return out, common.ErrorsOr(err, tagError(tagging.Check(out)))
}

func tagError(err error) error {
	switch {
	case errors.Is(err, tagging.ErrLimit):
		return awserror.New("TooManyTagsError", err.Error())
	case err != nil:
		return validation("%s", err.Error())
	}

	return nil
}

func (s *Service) GetParameter(ctx context.Context, in *api.GetParameterRequest) (*api.GetParameterResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, validation("Name is required.")
	}

	decrypt := in.WithDecryption != nil && *in.WithDecryption
	var out *api.GetParameterResult
	var got *version
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, v, selector, resolveErr := s.resolve(tx, m.Scope, *in.Name)
		if resolveErr != nil {
			return resolveErr
		}

		param := v.parameter(m.Scope, p.Name, selector)
		out, got = &api.GetParameterResult{Parameter: &param}, v
		return nil
	})
	if err != nil {
		return nil, err
	}

	if decrypt && got.Type == typeSecureString {
		item := revealed{
			version: got,
			name:    *out.Parameter.Name,
			set:     func(plain string) { out.Parameter.Value = &plain },
		}
		if keyErr := s.reveal(ctx, m.Scope, []revealed{item}); keyErr != nil {
			return nil, keyErr
		}
	}

	return out, nil
}

// checkBatch requires 1 to 10 names.
func checkBatch(names []string) error {
	if len(names) == 0 || len(names) > maxBatch {
		return validation("Names must list 1 to %d parameters.", maxBatch)
	}

	return nil
}

// GetParameters returns the parameters it finds and lists the names it does not.
func (s *Service) GetParameters(ctx context.Context, in *api.GetParametersRequest) (*api.GetParametersResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkBatch(in.Names); guardErr != nil {
		return nil, guardErr
	}

	out := &api.GetParametersResult{Parameters: []api.Parameter{}, InvalidParameters: []string{}}
	decrypt := in.WithDecryption != nil && *in.WithDecryption
	var secrets []revealed
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		seen := map[string]bool{}
		for _, name := range in.Names {
			if seen[name] {
				continue
			}

			seen[name] = true
			p, v, selector, resolveErr := s.resolve(tx, m.Scope, name)
			if _, isAWS := errors.AsType[*awserror.Error](resolveErr); isAWS {
				out.InvalidParameters = append(out.InvalidParameters, name)
				continue
			}

			if resolveErr != nil {
				return resolveErr
			}

			out.Parameters = append(out.Parameters, v.parameter(m.Scope, p.Name, selector))
			if decrypt && v.Type == typeSecureString {
				i := len(out.Parameters) - 1
				secrets = append(secrets, revealed{
					version: v, name: p.Name, set: func(plain string) { out.Parameters[i].Value = &plain },
				})
			}
		}

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

// deleteParameter removes a parameter with every version, label, and tag.
func deleteParameter(tx storage.WriteTx, scope request.Scope, name string) {
	for _, k := range tx.Keys(versionPrefix(scope, name)) {
		tx.Delete(k)
	}

	tx.Delete(parameterKey(scope, name))
	tx.Delete(expirationKey(scope, name))
}

func (s *Service) DeleteParameter(
	ctx context.Context,
	in *api.DeleteParameterRequest,
) (*api.DeleteParameterResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName(in.Name); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadParameter(tx, m.Scope, *in.Name); loadErr != nil {
			return loadErr
		}

		deleteParameter(tx, m.Scope, *in.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteParameterResult{}, nil
}

// DeleteParameters deletes the parameters it finds and lists the names it does not.
func (s *Service) DeleteParameters(
	ctx context.Context,
	in *api.DeleteParametersRequest,
) (*api.DeleteParametersResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkBatch(in.Names); guardErr != nil {
		return nil, guardErr
	}

	out := &api.DeleteParametersResult{DeletedParameters: []string{}, InvalidParameters: []string{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range in.Names {
			if slices.Contains(out.DeletedParameters, name) || slices.Contains(out.InvalidParameters, name) {
				continue
			}

			if _, ok := tx.Get(parameterKey(m.Scope, name)); !ok || checkName(&name) != nil {
				out.InvalidParameters = append(out.InvalidParameters, name)
				continue
			}

			deleteParameter(tx, m.Scope, name)
			out.DeletedParameters = append(out.DeletedParameters, name)
		}

		return nil
	})

	return out, err
}
