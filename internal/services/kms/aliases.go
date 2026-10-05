package kms

import (
	"context"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// alias is an alias record. Times are Unix milliseconds.
type alias struct {
	Name    string // without the alias/ prefix
	Target  string // key ID
	Created int64
	Updated int64
}

var aliasName = regexp.MustCompile(`^alias/[a-zA-Z0-9/_-]{1,250}$`)

func aliasKey(scope request.Scope, name string) string { return prefix(scope, "alias") + name }

func aliasARN(scope request.Scope, name string) string {
	return "arn:aws:kms:" + scope.Region + ":" + scope.AccountID + ":alias/" + name
}

// checkAliasName validates an alias name and returns it without the alias/ prefix.
func checkAliasName(name *string) (string, error) {
	full := common.Value(name)
	if !aliasName.MatchString(full) {
		return "", awserror.New("InvalidAliasNameException",
			"Alias must start with alias/ and hold 1 to 250 letters, digits, and / _ -.")
	}

	if strings.HasPrefix(full, "alias/aws/") {
		return "", awserror.New("InvalidAliasNameException", "Alias names starting with alias/aws/ are reserved.")
	}

	return strings.TrimPrefix(full, "alias/"), nil
}

// loadAlias reads an alias whose key still exists.
func (s *Service) loadAlias(tx storage.ReadTx, scope request.Scope, name string) (*alias, bool, error) {
	a, found, err := storage.Read[alias](tx, aliasKey(scope, name))
	if err != nil || !found {
		return nil, false, err
	}

	k, found, err := storage.Read[key](tx, keyKey(scope, a.Target))
	if err != nil || !found || k.deleted(s.now()) {
		return nil, false, err
	}

	return a, true, nil
}

// target loads the key an alias may point to: a key ID or ARN of a key that is
// not pending deletion.
func (s *Service) target(tx storage.ReadTx, scope request.Scope, ref *string) (*key, error) {
	k, err := s.load(tx, scope, ref, false)
	if err != nil {
		return nil, err
	}

	if activeErr := k.active(scope); activeErr != nil {
		return nil, activeErr
	}

	return k, nil
}

func (s *Service) CreateAlias(ctx context.Context, in *api.CreateAliasRequest) (*api.CreateAliasOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := checkAliasName(in.AliasName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists, loadErr := s.loadAlias(tx, scope, name); loadErr != nil || exists {
			if loadErr != nil {
				return loadErr
			}

			return awserror.New("AlreadyExistsException",
				"An alias with the name "+aliasARN(scope, name)+" already exists")
		}

		k, targetErr := s.target(tx, scope, in.TargetKeyId)
		if targetErr != nil {
			return targetErr
		}

		now := s.now().UnixMilli()
		return storage.Write(tx, aliasKey(scope, name), &alias{Name: name, Target: k.ID, Created: now, Updated: now})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAliasOutput{}, nil
}

func (s *Service) UpdateAlias(ctx context.Context, in *api.UpdateAliasRequest) (*api.UpdateAliasOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := checkAliasName(in.AliasName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, exists, loadErr := s.loadAlias(tx, scope, name)
		if loadErr != nil {
			return loadErr
		}

		if !exists {
			return notFound("Alias %s is not found.", aliasARN(scope, name))
		}

		k, targetErr := s.target(tx, scope, in.TargetKeyId)
		if targetErr != nil {
			return targetErr
		}

		a.Target, a.Updated = k.ID, s.now().UnixMilli()
		return storage.Write(tx, aliasKey(scope, name), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAliasOutput{}, nil
}

func (s *Service) DeleteAlias(ctx context.Context, in *api.DeleteAliasRequest) (*api.DeleteAliasOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := checkAliasName(in.AliasName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, exists, loadErr := s.loadAlias(tx, scope, name)
		if loadErr != nil {
			return loadErr
		}

		if !exists {
			return notFound("Alias %s is not found.", aliasARN(scope, name))
		}

		tx.Delete(aliasKey(scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAliasOutput{}, nil
}

// ListAliases lists the caller's aliases in name order, or only those of KeyId.
func (s *Service) ListAliases(ctx context.Context, in *api.ListAliasesRequest) (*api.ListAliasesResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var aliases []*alias
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		only := ""
		if in.KeyId != nil {
			k, loadErr := s.load(tx, scope, in.KeyId, false)
			if loadErr != nil {
				return loadErr
			}

			only = k.ID
		}

		records := prefix(scope, "alias")
		for _, k := range tx.Keys(records) {
			a, exists, loadErr := s.loadAlias(tx, scope, strings.TrimPrefix(k, records))
			if loadErr != nil {
				return loadErr
			}

			if exists && (only == "" || a.Target == only) {
				aliases = append(aliases, a)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	aliases, next, err := page(aliases, func(a *alias) string { return a.Name }, in.Marker, in.Limit, 50, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListAliasesResponse{Aliases: []api.AliasListEntry{}, NextMarker: next, Truncated: new(next != nil)}
	for _, a := range aliases {
		out.Aliases = append(out.Aliases, api.AliasListEntry{
			AliasName: new("alias/" + a.Name), AliasArn: new(aliasARN(scope, a.Name)), TargetKeyId: new(a.Target),
			CreationDate: seconds(a.Created), LastUpdatedDate: seconds(a.Updated),
		})
	}

	return out, nil
}
