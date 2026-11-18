package keyspaces

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func definedTypes(tx storage.ReadTx, scope request.Scope, ks string) (map[string]int64, error) {
	records, err := storage.Records[record](tx, childKey(scope, kindType, ks, ""))
	if err != nil {
		return nil, err
	}

	defined := make(map[string]int64, len(records))
	for _, r := range records {
		defined[r.Name] = r.Depth
	}

	return defined, nil
}

func validateFields(fields []api.FieldDefinition, defined map[string]int64) (
	refs []string, depth int64, err error,
) {
	if len(fields) == 0 {
		return nil, 0, awserror.Validation("A user-defined type requires at least one field")
	}

	names := map[string]bool{}
	for _, field := range fields {
		name := common.Value(field.Name)
		if !columnName.MatchString(name) || names[name] {
			return nil, 0, awserror.Validation("Invalid or duplicate field %q", name)
		}

		names[name] = true
		ct, err := parseType(common.Value(field.Type), defined)
		if err != nil {
			return nil, 0, err
		}

		if ct.multiCell || ct.counter {
			return nil, 0, awserror.Validation(
				"Field %q requires a single-cell, non-counter type; use frozen collections or UDTs",
				name,
			)
		}

		for _, ref := range ct.refs {
			depth = max(depth, defined[ref]+1)
		}

		refs = append(refs, ct.refs...)
	}

	slices.Sort(refs)
	return slices.Compact(refs), depth, nil
}

func (s *Service) CreateType(ctx context.Context, in *api.CreateTypeRequest) (*api.CreateTypeResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks := common.Value(in.KeyspaceName)
	if writableErr := writable(ks); writableErr != nil {
		return nil, writableErr
	}

	name, err := typeName(common.Value(in.TypeName))
	if err != nil {
		return nil, err
	}

	key := childKey(scope, kindType, ks, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireKeyspace(tx, scope, ks); e != nil {
			return e
		}

		if e := createRecord(tx, key); e != nil {
			return e
		}

		defined, e := definedTypes(tx, scope, ks)
		if e != nil {
			return e
		}

		refs, depth, e := validateFields(in.FieldDefinitions, defined)
		if e != nil {
			return e
		}

		r := &record{
			Keyspace: ks,
			Name:     name,
			Fields:   in.FieldDefinitions,
			Created:  time.Now().UTC(),
			TypeRefs: refs,
			Depth:    depth,
		}
		statement, e := createTypeCQL(r, defined)
		if e != nil {
			return e
		}

		return s.writeChange(tx, scope, key, r, creating, statement)
	})
	return &api.CreateTypeResponse{KeyspaceArn: new(resourceARN(scope, ks, "")), TypeName: new(name)}, err
}

func (s *Service) GetType(ctx context.Context, in *api.GetTypeRequest) (*api.GetTypeResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := typeName(common.Value(in.TypeName))
	if err != nil {
		return nil, err
	}

	ks := common.Value(in.KeyspaceName)
	var out *api.GetTypeResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readRecord(tx, childKey(scope, kindType, ks, name))
		if e != nil {
			return e
		}

		tables, parents, e := referringRecords(tx, scope, ks, name)
		if e != nil {
			return e
		}

		out = s.typeOutput(scope, r)
		out.DirectReferringTables, out.DirectParentTypes = tables, parents
		return nil
	})
	return out, err
}

func (s *Service) typeOutput(scope request.Scope, r *record) *api.GetTypeResponse {
	return &api.GetTypeResponse{
		KeyspaceName: new(r.Keyspace), TypeName: new(r.Name), KeyspaceArn: new(resourceARN(scope, r.Keyspace, "")),
		FieldDefinitions: r.Fields, LastModifiedTimestamp: common.Seconds(r.Created), Status: new(s.status(r)),
		MaxNestingDepth: new(r.Depth),
	}
}

func referringRecords(tx storage.ReadTx, scope request.Scope, ks, name string) (
	tables, parents []string, err error,
) {
	var refs [2][]string
	for i, kind := range []string{kindTable, kindType} {
		records, err := storage.Records[record](tx, childKey(scope, kind, ks, ""))
		if err != nil {
			return nil, nil, err
		}

		for _, r := range records {
			if slices.Contains(r.TypeRefs, name) {
				refs[i] = append(refs[i], r.Name)
			}
		}
	}

	return refs[0], refs[1], nil
}

func (s *Service) DeleteType(ctx context.Context, in *api.DeleteTypeRequest) (*api.DeleteTypeResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks := common.Value(in.KeyspaceName)
	if writableErr := writable(ks); writableErr != nil {
		return nil, writableErr
	}

	name, err := typeName(common.Value(in.TypeName))
	if err != nil {
		return nil, err
	}

	key := childKey(scope, kindType, ks, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readRecord(tx, key); e != nil {
			return e
		}

		tables, parents, e := referringRecords(tx, scope, ks, name)
		if e != nil {
			return e
		}

		if len(tables) != 0 || len(parents) != 0 {
			return conflict("The user-defined type is still referenced")
		}

		return s.writeChange(tx, scope, key, nil, "", "DROP TYPE IF EXISTS "+qualified(ks, name))
	})
	return &api.DeleteTypeResponse{KeyspaceArn: new(resourceARN(scope, ks, "")), TypeName: new(name)}, err
}

func (s *Service) ListTypes(ctx context.Context, in *api.ListTypesRequest) (*api.ListTypesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks := common.Value(in.KeyspaceName)
	names := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := requireKeyspace(tx, scope, ks); e != nil {
			return e
		}

		records, e := storage.Records[record](tx, childKey(scope, kindType, ks, ""))
		if e != nil {
			return e
		}

		for _, r := range records {
			names = append(names, r.Name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := resourcePage(names, func(name string) string { return name }, in.NextToken, in.MaxResults)
	return &api.ListTypesResponse{Types: page, NextToken: next}, err
}
