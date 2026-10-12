package cloudformation

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/s3tables"
)

const (
	s3TableBucketType    = "AWS::S3Tables::TableBucket"
	s3NamespaceType      = "AWS::S3Tables::Namespace"
	s3TableType          = "AWS::S3Tables::Table"
	s3BucketName         = "TableBucketName"
	s3BucketARN          = "TableBucketARN"
	s3Namespace          = "Namespace"
	s3TableName          = "TableName"
	s3TableARN           = "TableARN"
	s3Format             = "OpenTableFormat"
	s3WithoutMetadata    = "WithoutMetadata"
	s3Version            = "VersionToken"
	s3Previous           = "S3TablesPreviousRecord"
	s3PreviousProperties = "S3TablesPreviousProperties"
	s3Committed          = "S3TablesCommittedRevision"
)

var s3tablesCFTag = regexp.MustCompile(`^[\p{L}\p{N} _./=+@-]*$`)

type s3tablesRecords interface {
	CreateTableBucket(context.Context, *api.CreateTableBucketRequest) (*api.CreateTableBucketResponse, error)
	GetTableBucket(context.Context, *api.GetTableBucketRequest) (*api.GetTableBucketResponse, error)
	DeleteTableBucket(context.Context, *api.DeleteTableBucketRequest) (*api.DeleteTableBucketOutput, error)
	CreateNamespace(context.Context, *api.CreateNamespaceRequest) (*api.CreateNamespaceResponse, error)
	GetNamespace(context.Context, *api.GetNamespaceRequest) (*api.GetNamespaceResponse, error)
	DeleteNamespace(context.Context, *api.DeleteNamespaceRequest) (*api.DeleteNamespaceOutput, error)
	CreateTable(context.Context, *api.CreateTableRequest) (*api.CreateTableResponse, error)
	GetTable(context.Context, *api.GetTableRequest) (*api.GetTableResponse, error)
	DeleteTable(context.Context, *api.DeleteTableRequest) (*api.DeleteTableOutput, error)
	CloudFormationUpdateBucket(
		context.Context,
		string,
		map[string]string,
		map[string]string,
	) (*s3tables.Snapshot, *s3tables.Snapshot, error)
	CloudFormationUpdateTable(
		context.Context,
		*api.RenameTableRequest,
		map[string]string,
		map[string]string,
	) (*s3tables.Snapshot, *s3tables.Snapshot, error)
	CloudFormationRestore(context.Context, *s3tables.Snapshot, string) (*s3tables.Snapshot, error)
}

func (s *Service) UseS3Tables(records s3tablesRecords) { s.s3tables = records }
func isS3TablesResource(typ string) bool {
	return typ == s3TableBucketType || typ == s3NamespaceType || typ == s3TableType
}

func (c *client) s3tablesContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "s3tables", Endpoint: c.endpoint})
}

type s3tablesProperties struct {
	tags                            map[string]string
	bucket, name, namespace, format string
}

func s3tablesParams(typ string, p props, scope request.Scope) (*s3tablesProperties, error) {
	allowed := []string{s3BucketName, memberTags}
	required := []string{s3BucketName}
	if typ == s3NamespaceType {
		allowed = []string{s3BucketARN, s3Namespace}
		required = allowed
	}
	if typ == s3TableType {
		allowed = []string{s3BucketARN, s3Namespace, s3TableName, s3Format, s3WithoutMetadata, memberTags}
		required = []string{s3BucketARN, s3Namespace, s3TableName, s3Format, s3WithoutMetadata}
	}
	for k, v := range p {
		if !slices.Contains(allowed, k) || v == nil {
			return nil, validation("Invalid or unsupported S3 Tables property %s", k)
		}
		if k != memberTags {
			if _, ok := v.(string); !ok {
				return nil, validation("S3 Tables %s requires a string", k)
			}
		}
	}
	for _, k := range required {
		if _, ok := p[k]; !ok {
			return nil, validation("S3 Tables %s is required in this subset", k)
		}
	}
	out := &s3tablesProperties{
		bucket:    p.str(s3BucketARN),
		name:      p.str(s3BucketName),
		namespace: p.str(s3Namespace),
		format:    p.str(s3Format),
		tags:      map[string]string{},
	}
	if typ == s3TableBucketType {
		if e := s3tables.ValidateBucketName(out.name); e != nil {
			return nil, e
		}
	} else {
		prefix := "arn:aws:s3tables:" + scope.Region + ":" + scope.AccountID + ":bucket/"
		name, ok := strings.CutPrefix(out.bucket, prefix)
		if !ok || s3tables.ValidateBucketName(name) != nil {
			return nil, validation("TableBucketARN requires an exact S3 Tables bucket ARN in this account and Region")
		}
		if e := s3tables.ValidateNamespaceName(out.namespace); e != nil {
			return nil, e
		}
	}
	if typ == s3TableType {
		out.name = p.str(s3TableName)
		if e := s3tables.ValidateTableName(out.name); e != nil {
			return nil, e
		}
		if out.format != "ICEBERG" || p.str(s3WithoutMetadata) != "Yes" {
			return nil, validation("S3 Tables Table requires ICEBERG and WithoutMetadata: Yes")
		}
	}
	if v, has := p[memberTags]; has {
		list, ok := v.([]any)
		if !ok {
			return nil, validation("Tags requires Key/Value objects")
		}
		for _, v := range list {
			m, ok := v.(map[string]any)
			if !ok || len(m) != 2 {
				return nil, validation("Tag requires exactly Key and Value")
			}
			k, ok := m[memberKey].(string)
			if !ok {
				return nil, validation("Tag Key requires a string")
			}
			val, ok := m[memberValue].(string)
			if !ok {
				return nil, validation("Tag Value requires a string")
			}
			if _, ok := out.tags[k]; ok {
				return nil, validation("Duplicate tag key")
			}
			if !s3tablesCFTag.MatchString(k) || !s3tablesCFTag.MatchString(val) {
				return nil, validation(
					"S3 Tables CloudFormation tags require alphanumeric characters, spaces or _ . / = + - @",
				)
			}
			out.tags[k] = val
		}
	}
	if e := s3tables.ValidateTags(out.tags); e != nil {
		return nil, e
	}
	return out, nil
}

func s3tablesAttributes(r *s3tables.Snapshot) map[string]any {
	if r.Bucket != nil {
		return map[string]any{s3BucketARN: *r.Bucket.Arn}
	}
	t := r.Table
	return map[string]any{
		s3TableARN:  *t.TableARN,
		s3Version:   *t.VersionToken,
		s3TableName: *t.Name,
		s3BucketARN: r.BucketARN,
		s3Namespace: t.Namespace[0],
	}
}

func s3tablesSnapshotAttributes(r *resource, before, after *s3tables.Snapshot) (map[string]any, error) {
	raw, e := json.Marshal(before)
	if e != nil {
		return nil, e
	}
	var snapshot map[string]any
	if checkErr := json.Unmarshal(raw, &snapshot); checkErr != nil {
		return nil, checkErr
	}
	attrs := s3tablesAttributes(after)
	attrs[s3Previous] = snapshot
	attrs[s3PreviousProperties] = maps.Clone(r.Properties)
	attrs[s3Committed] = after.Revision
	return attrs, nil
}

func init() {
	register("AWS::S3Tables::TableBucket", &resourceType{
		properties: []string{
			s3BucketName,
			memberTags,
		},
		replace:    []string{s3BucketName},
		attributes: []string{s3BucketARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := s3tablesParams(s3TableBucketType, p, c.scope)
			if e != nil {
				return "", nil, e
			}
			if c.s3tables == nil {
				return "", nil, validation("S3 Tables adapter is unavailable")
			}
			out, e := c.s3tables.CreateTableBucket(
				c.s3tablesContext(ctx),
				&api.CreateTableBucketRequest{Name: new(in.name), Tags: in.tags},
			)
			if e != nil {
				return "", nil, e
			}
			return in.name, map[string]any{s3BucketARN: *out.Arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := s3tablesParams(r.Type, p, c.scope)
			if e != nil {
				return nil, e
			}
			old, e := s3tablesParams(r.Type, props(r.Properties), c.scope)
			if e != nil {
				return nil, e
			}
			before, after, e := c.s3tables.CloudFormationUpdateBucket(
				c.s3tablesContext(ctx),
				r.Attributes[s3BucketARN].(string),
				old.tags,
				in.tags,
			)
			if e != nil {
				return nil, e
			}
			return s3tablesSnapshotAttributes(r, before, after)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.s3tables.DeleteTableBucket(
				c.s3tablesContext(ctx),
				&api.DeleteTableBucketRequest{TableBucketARN: new(r.Attributes[s3BucketARN].(string))},
			)
			if gone(e, "NotFoundException") {
				return nil
			}
			return e
		},
	})
	register("AWS::S3Tables::Namespace", &resourceType{
		properties: []string{s3BucketARN, s3Namespace}, replace: []string{s3BucketARN, s3Namespace},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := s3tablesParams(s3NamespaceType, p, c.scope)
			if e != nil {
				return "", nil, e
			}
			if c.s3tables == nil {
				return "", nil, validation("S3 Tables adapter is unavailable")
			}
			_, e = c.s3tables.CreateNamespace(
				c.s3tablesContext(ctx),
				&api.CreateNamespaceRequest{TableBucketARN: new(in.bucket), Namespace: []string{in.namespace}},
			)
			if e != nil {
				return "", nil, e
			}
			return in.namespace, map[string]any{s3BucketARN: in.bucket, s3Namespace: in.namespace}, nil
		},
		update: func(_ context.Context, _ *client, r *resource, _ props) (map[string]any, error) {
			return maps.Clone(r.Attributes), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.s3tables.DeleteNamespace(
				c.s3tablesContext(ctx),
				&api.DeleteNamespaceRequest{
					TableBucketARN: new(r.Attributes[s3BucketARN].(string)),
					Namespace:      new(r.Attributes[s3Namespace].(string)),
				},
			)
			if gone(e, "NotFoundException") {
				return nil
			}
			return e
		},
	})
	register("AWS::S3Tables::Table", &resourceType{
		properties: []string{
			s3BucketARN,
			s3Namespace,
			s3TableName,
			s3Format,
			s3WithoutMetadata,
			memberTags,
		},
		replace:    []string{s3BucketARN, s3Format, s3WithoutMetadata},
		attributes: []string{s3TableARN, s3Version},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := s3tablesParams(s3TableType, p, c.scope)
			if e != nil {
				return "", nil, e
			}
			if c.s3tables == nil {
				return "", nil, validation("S3 Tables adapter is unavailable")
			}
			out, e := c.s3tables.CreateTable(
				c.s3tablesContext(ctx),
				&api.CreateTableRequest{
					TableBucketARN: new(in.bucket),
					Namespace:      new(in.namespace),
					Name:           new(in.name),
					Format:         new(in.format),
					Tags:           in.tags,
				},
			)
			if e != nil {
				return "", nil, e
			}
			return in.name, map[string]any{
				s3TableARN:  *out.TableARN,
				s3Version:   *out.VersionToken,
				s3BucketARN: in.bucket,
				s3Namespace: in.namespace,
				s3TableName: in.name,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := s3tablesParams(r.Type, p, c.scope)
			if e != nil {
				return nil, e
			}
			old, e := s3tablesParams(r.Type, props(r.Properties), c.scope)
			if e != nil {
				return nil, e
			}
			live, e := c.s3tables.GetTable(
				c.s3tablesContext(ctx),
				&api.GetTableRequest{TableArn: new(r.Attributes[s3TableARN].(string))},
			)
			if e != nil {
				return nil, e
			}
			before, after, e := c.s3tables.CloudFormationUpdateTable(
				c.s3tablesContext(ctx),
				&api.RenameTableRequest{
					TableBucketARN:   new(r.Attributes[s3BucketARN].(string)),
					Namespace:        new(live.Namespace[0]),
					Name:             live.Name,
					NewNamespaceName: new(in.namespace),
					NewName:          new(in.name),
					VersionToken:     live.VersionToken,
				},
				old.tags,
				in.tags,
			)
			if e != nil {
				return nil, e
			}
			return s3tablesSnapshotAttributes(r, before, after)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			live, e := c.s3tables.GetTable(
				c.s3tablesContext(ctx),
				&api.GetTableRequest{TableArn: new(r.Attributes[s3TableARN].(string))},
			)
			if gone(e, "NotFoundException") {
				return nil
			}
			if e != nil {
				return e
			}
			_, e = c.s3tables.DeleteTable(
				c.s3tablesContext(ctx),
				&api.DeleteTableRequest{
					TableBucketARN: new(r.Attributes[s3BucketARN].(string)),
					Namespace:      new(live.Namespace[0]),
					Name:           live.Name,
					VersionToken:   live.VersionToken,
				},
			)
			if gone(e, "NotFoundException") {
				return nil
			}
			return e
		},
	})
}

func s3tablesRead(ctx context.Context, c *client, r *resource) (map[string]any, error) {
	switch r.Type {
	case s3TableBucketType:
		out, e := c.s3tables.GetTableBucket(
			c.s3tablesContext(ctx),
			&api.GetTableBucketRequest{TableBucketARN: new(r.Attributes[s3BucketARN].(string))},
		)
		if e != nil {
			return nil, e
		}
		return s3tablesAttributes(&s3tables.Snapshot{Bucket: out}), nil
	case s3NamespaceType:
		_, e := c.s3tables.GetNamespace(
			c.s3tablesContext(ctx),
			&api.GetNamespaceRequest{
				TableBucketARN: new(r.Attributes[s3BucketARN].(string)),
				Namespace:      new(r.Attributes[s3Namespace].(string)),
			},
		)
		return maps.Clone(r.Attributes), e
	default:
		out, e := c.s3tables.GetTable(
			c.s3tablesContext(ctx),
			&api.GetTableRequest{TableArn: new(r.Attributes[s3TableARN].(string))},
		)
		if e != nil {
			return nil, e
		}
		return s3tablesAttributes(&s3tables.Snapshot{Table: out, BucketARN: r.Attributes[s3BucketARN].(string)}), nil
	}
}

func s3tablesRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || !isS3TablesResource(next.typ) {
		return "", nil, false
	}
	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}
	return "", nil, false
}

func s3tablesRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	b, ok := r.Attributes[s3Previous].(map[string]any)
	previous, has := r.Attributes[s3PreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return nil, true, e
	}
	var snapshot s3tables.Snapshot
	if checkErr := json.Unmarshal(raw, &snapshot); checkErr != nil {
		return nil, true, checkErr
	}
	rev, ok := r.Attributes[s3Committed].(string)
	if !ok {
		return nil, true, validation("Missing S3 Tables compensation revision")
	}
	after, e := c.s3tables.CloudFormationRestore(c.s3tablesContext(ctx), &snapshot, rev)
	if e != nil {
		return nil, true, e
	}
	return s3tablesAttributes(after), true, nil
}

func sameS3TablesIdentity(a, b *resource) bool {
	if a == nil || b == nil || a.Type != b.Type {
		return false
	}
	if a.Type == s3TableType {
		return a.Attributes[s3TableARN] == b.Attributes[s3TableARN]
	}
	return a.Physical == b.Physical && a.Attributes[s3BucketARN] == b.Attributes[s3BucketARN]
}

// s3tablesOrderRetired retires this hierarchy's children before their parents.
// Other services retain their existing relative order.
func s3tablesOrderRetired(st *stack) {
	for i := 0; i < len(st.Replaced); i++ {
		parent := st.Replaced[i].Resource
		if parent.Type != s3TableBucketType && parent.Type != s3NamespaceType {
			continue
		}
		for j := i + 1; j < len(st.Replaced); j++ {
			child := st.Replaced[j].Resource
			if !isS3TablesResource(child.Type) || child.Type == s3TableBucketType ||
				child.Attributes[s3BucketARN] != parent.Attributes[s3BucketARN] {
				continue
			}
			if parent.Type == s3NamespaceType &&
				(child.Type != s3TableType || child.Attributes[s3Namespace] != parent.Attributes[s3Namespace]) {
				continue
			}
			item := st.Replaced[j]
			copy(st.Replaced[i+1:j+1], st.Replaced[i:j])
			st.Replaced[i] = item
			i--
			break
		}
	}
}
