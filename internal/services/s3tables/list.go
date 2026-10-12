package s3tables

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type cursor struct {
	Binding     string
	After       string
	Incarnation string
	Checksum    string
}
type listOptions struct {
	token                                     *string
	max                                       *int64
	api, kind, bucket, namespace, prefix, typ string
}

func pageLimit(p *int64) (int, error) {
	n := int64(1000)
	if p != nil {
		n = *p
	}
	if n < 1 || n > 1000 {
		return 0, invalid("Page limit requires 1-1000; local default 1000")
	}
	return int(n), nil
}

func digest(parts ...string) string {
	b, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func encodeCursor(c cursor) string {
	c.Checksum = digest(c.Binding, c.After, c.Incarnation)
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(p *string, binding, prefix string) (cursor, error) {
	c := cursor{}
	if p == nil {
		return c, nil
	}
	if len(*p) < 1 || len(*p) > 2048 {
		return c, invalid("Invalid continuationToken length")
	}
	b, e := base64.RawURLEncoding.DecodeString(*p)
	if e != nil {
		return c, invalid("Invalid continuationToken encoding")
	}
	if e = json.Unmarshal(
		b,
		&c,
	); e != nil || encodeCursor(c) != *p || c.Binding != binding ||
		!strings.HasPrefix(c.After, prefix) || c.After == prefix ||
		c.Checksum != digest(c.Binding, c.After, c.Incarnation) {
		return c, invalid("Invalid continuationToken for scope, API, parents or filters")
	}
	u, e := uuid.Parse(c.Incarnation)
	if e != nil || u.String() != c.Incarnation {
		return c, invalid("Invalid continuationToken incarnation")
	}
	return c, nil
}

// list reads parents and the page in one snapshot. Deleted cursors resume by
// ordered key; recreated cursor records or parents invalidate an old token.
func (s *Service) list(ctx context.Context, o *listOptions) ([]*Snapshot, *string, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	limit, e := pageLimit(o.max)
	if e != nil {
		return nil, nil, e
	}
	if !utf8.ValidString(o.prefix) {
		return nil, nil, invalid("prefix requires valid UTF-8")
	}
	records := []*Snapshot{}
	var next *string
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		parent, nsParent, b := "", "", ""
		if o.kind != bucketKind {
			r, err := readBucket(tx, scope, o.bucket)
			if err != nil {
				return err
			}
			parent = r.Incarnation
			b = *r.Bucket.Name
			if o.namespace != "" {
				n, err := readNamespace(tx, scope, o.bucket, o.namespace)
				if err != nil {
					return err
				}
				nsParent = n.Incarnation
			}
		}
		prefix := scope.Prefix(serviceName, o.kind)
		if b != "" {
			prefix = key(scope, o.kind, b, "") + "/"
		}
		if o.namespace != "" {
			prefix = key(scope, o.kind, b, o.namespace) + "/"
		}
		binding := digest(
			scope.AccountID,
			scope.Region,
			o.api,
			o.bucket,
			parent,
			o.namespace,
			nsParent,
			o.prefix,
			o.typ,
		)
		c, err := decodeCursor(o.token, binding, prefix)
		if err != nil {
			return err
		}
		if c.After != "" {
			r, ok, err := storage.Read[Snapshot](tx, c.After)
			if err != nil {
				return err
			}
			if ok && r.Incarnation != c.Incarnation {
				return invalid("continuationToken cursor record was recreated")
			}
		}
		last := cursor{Binding: binding}
		for _, k := range tx.Keys(prefix) {
			r, err := read(tx, k)
			if err != nil {
				return err
			}
			name := ""
			switch o.kind {
			case bucketKind:
				name = *r.Bucket.Name
			case namespaceKind:
				name = r.Namespace.Namespace[0]
			case tableKind:
				name = *r.Table.Name
			}
			if k <= c.After || !strings.HasPrefix(name, o.prefix) {
				continue
			}
			if len(records) == limit {
				next = new(encodeCursor(last))
				break
			}
			records = append(records, r)
			last.After = k
			last.Incarnation = r.Incarnation
		}
		return nil
	})
	return records, next, e
}

func (s *Service) ListTableBuckets(
	ctx context.Context,
	in *api.ListTableBucketsRequest,
) (*api.ListTableBucketsResponse, error) {
	if in == nil {
		return nil, invalid("input is required")
	}
	typ := common.Value(in.Type)
	if in.Type != nil && typ != customer {
		return nil, invalid("Only customer table buckets are supported")
	}
	r, next, e := s.list(
		ctx,
		&listOptions{
			api:    "ListTableBuckets",
			kind:   bucketKind,
			prefix: common.Value(in.Prefix),
			typ:    typ,
			token:  copyString(in.ContinuationToken),
			max:    in.MaxBuckets,
		},
	)
	if e != nil {
		return nil, e
	}
	out := &api.ListTableBucketsResponse{TableBuckets: []api.TableBucketSummary{}, ContinuationToken: next}
	for _, v := range r {
		t := v.Bucket
		out.TableBuckets = append(
			out.TableBuckets,
			api.TableBucketSummary{
				Arn:            t.Arn,
				Name:           t.Name,
				OwnerAccountId: t.OwnerAccountId,
				CreatedAt:      t.CreatedAt,
				TableBucketId:  t.TableBucketId,
				Type:           t.Type,
			},
		)
	}
	return common.Clone(out), nil
}

func (s *Service) ListNamespaces(
	ctx context.Context,
	in *api.ListNamespacesRequest,
) (*api.ListNamespacesResponse, error) {
	if in == nil {
		return nil, invalid("input is required")
	}
	r, next, e := s.list(
		ctx,
		&listOptions{
			api:    "ListNamespaces",
			kind:   namespaceKind,
			bucket: common.Value(in.TableBucketARN),
			prefix: common.Value(in.Prefix),
			token:  copyString(in.ContinuationToken),
			max:    in.MaxNamespaces,
		},
	)
	if e != nil {
		return nil, e
	}
	out := &api.ListNamespacesResponse{Namespaces: []api.NamespaceSummary{}, ContinuationToken: next}
	for _, v := range r {
		t := v.Namespace
		out.Namespaces = append(
			out.Namespaces,
			api.NamespaceSummary{
				Namespace:      t.Namespace,
				CreatedAt:      t.CreatedAt,
				CreatedBy:      t.CreatedBy,
				OwnerAccountId: t.OwnerAccountId,
				NamespaceId:    t.NamespaceId,
				TableBucketId:  t.TableBucketId,
			},
		)
	}
	return common.Clone(out), nil
}

func (s *Service) ListTables(ctx context.Context, in *api.ListTablesRequest) (*api.ListTablesResponse, error) {
	if in == nil {
		return nil, invalid("input is required")
	}
	ns := common.Value(in.Namespace)
	if in.Namespace != nil {
		if e := ValidateNamespaceName(ns); e != nil {
			return nil, e
		}
	}
	r, next, e := s.list(
		ctx,
		&listOptions{
			api:       "ListTables",
			kind:      tableKind,
			bucket:    common.Value(in.TableBucketARN),
			namespace: ns,
			prefix:    common.Value(in.Prefix),
			token:     copyString(in.ContinuationToken),
			max:       in.MaxTables,
		},
	)
	if e != nil {
		return nil, e
	}
	out := &api.ListTablesResponse{Tables: []api.TableSummary{}, ContinuationToken: next}
	for _, v := range r {
		t := v.Table
		out.Tables = append(
			out.Tables,
			api.TableSummary{
				Name:          t.Name,
				Namespace:     t.Namespace,
				Type:          t.Type,
				TableARN:      t.TableARN,
				CreatedAt:     t.CreatedAt,
				ModifiedAt:    t.ModifiedAt,
				NamespaceId:   t.NamespaceId,
				TableBucketId: t.TableBucketId,
			},
		)
	}
	return common.Clone(out), nil
}
