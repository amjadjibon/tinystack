package ssm

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutParameter creates a parameter, or with Overwrite adds a version to one.
// Settings a new version leaves out keep their current values.
func (s *Service) PutParameter(ctx context.Context, in *api.PutParameterRequest) (*api.PutParameterResult, error) {
	m, err := identity(ctx)
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

	// A key on another type fails validation in settings.
	if value(in.Type, typeSecureString) == typeSecureString {
		if keyErr := s.checkNewKey(ctx, m.Scope, *in.Name, in.KeyId, overwrite); keyErr != nil {
			return nil, keyErr
		}
	}

	now := s.now().UnixMilli()
	var out *api.PutParameterResult
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pk := parameterKey(m.Scope, *in.Name)
		p, exists, readErr := read[parameter](tx, pk)
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

		next, settingsErr := settings(in, previous)
		if settingsErr != nil {
			return settingsErr
		}

		next.Version, next.ModifiedAt = p.Version+1, now
		if pruneErr := makeRoom(tx, m.Scope, p); pruneErr != nil {
			return pruneErr
		}

		p.Version = next.Version
		if writeErr := write(tx, versionKey(m.Scope, p.Name, next.Version), next); writeErr != nil {
			return writeErr
		}

		out = &api.PutParameterResult{Version: new(next.Version), Tier: new(next.Tier)}
		return write(tx, pk, p)
	})

	return out, err
}

// settings returns the version in describes, keeping previous's settings
// for those it leaves out.
func settings(in *api.PutParameterRequest, previous *version) (*version, error) {
	next := &version{
		Value:          *in.Value,
		Type:           previous.Type,
		KeyID:          previous.KeyID,
		Description:    previous.Description,
		AllowedPattern: previous.AllowedPattern,
		Tier:           previous.Tier,
		DataType:       previous.DataType,
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

	if err := checkTier(in.Tier, previous.Tier, next); err != nil {
		return nil, err
	}

	if next.Type == typeSecureString {
		ciphertext := make([]byte, len(next.Value)+32)
		_, _ = rand.Read(ciphertext)
		next.Ciphertext = base64.StdEncoding.EncodeToString(ciphertext)
	}

	return next, nil
}

func value(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}

	return *p
}

// checkNewKey checks the key a SecureString version will use: the requested
// one, or on overwrite the current version's.
func (s *Service) checkNewKey(ctx context.Context, scope request.Scope, name string, keyID *string,
	overwrite bool,
) error {
	if keyID != nil {
		return s.useKey(ctx, scope, *keyID)
	}

	if !overwrite {
		return nil
	}

	var current string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		p, found, readErr := read[parameter](tx, parameterKey(scope, name))
		if readErr != nil || !found {
			return readErr
		}

		v, _, readErr := loadVersion(tx, scope, name, p.Version)
		if readErr == nil {
			current = v.KeyID
		}

		return readErr
	})
	if err != nil || current == "" {
		return err
	}

	return s.useKey(ctx, scope, current)
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
		if len(v.Value) > maxStandardValue || previous == tierAdvanced {
			tier = tierAdvanced
		}
	case tierStandard, tierAdvanced:
	default:
		return validation("Tier must be Standard, Advanced, or Intelligent-Tiering.")
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

	oldest, _, err := read[version](tx, keys[0])
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
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key == nil || t.Value == nil {
			return nil, validation("Tags must have a Key and a Value.")
		}

		out[*t.Key] = *t.Value
	}

	return out, tagError(tagging.Check(out))
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
	m, err := identity(ctx)
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

		param := v.parameter(m.Scope, p.Name, selector, decrypt)
		out, got = &api.GetParameterResult{Parameter: &param}, v
		return nil
	})
	if err != nil {
		return nil, err
	}

	if decrypt {
		if keyErr := s.decrypting(ctx, m.Scope, []*version{got}); keyErr != nil {
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
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkBatch(in.Names); guardErr != nil {
		return nil, guardErr
	}

	out := &api.GetParametersResult{Parameters: []api.Parameter{}, InvalidParameters: []string{}}
	decrypt := in.WithDecryption != nil && *in.WithDecryption
	var versions []*version
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

			out.Parameters = append(out.Parameters, v.parameter(m.Scope, p.Name, selector, decrypt))
			versions = append(versions, v)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if decrypt {
		if keyErr := s.decrypting(ctx, m.Scope, versions); keyErr != nil {
			return nil, keyErr
		}
	}

	return out, nil
}

// deleteParameter removes a parameter with every version, label, and tag.
func deleteParameter(tx storage.WriteTx, scope request.Scope, name string) {
	for _, k := range tx.Keys(versionPrefix(scope, name)) {
		tx.Delete(k)
	}

	tx.Delete(parameterKey(scope, name))
}

func (s *Service) DeleteParameter(
	ctx context.Context,
	in *api.DeleteParameterRequest,
) (*api.DeleteParameterResult, error) {
	m, err := identity(ctx)
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
	m, err := identity(ctx)
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
