package cloudformation

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	cassandraEnabled     = "ENABLED"
	cassandraDisabled    = "DISABLED"
	keyspacesService     = "keyspaces"
	keyspaceName         = "KeyspaceName"
	cassandraReplication = "ReplicationSpecification"
	cassandraPartition   = "PartitionKeyColumns"
	cassandraClustering  = "ClusteringKeyColumns"
	cassandraRegular     = "RegularColumns"
	cassandraEncryption  = "EncryptionSpecification"
	cassandraPITR        = "PointInTimeRecoveryEnabled"
	cassandraTTL         = "DefaultTimeToLive"
	cassandraTimestamps  = "ClientSideTimestampsEnabled"
)

func init() {
	register("AWS::Cassandra::Keyspace", &resourceType{
		name: keyspaceName, properties: []string{keyspaceName, cassandraReplication, memberTags},
		replace: []string{keyspaceName}, attributes: []string{memberArn, keyspaceName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := cassandraName(c, p, keyspaceName)
			params := pick(map[string]any{keyspaceName: name}, p, cassandraReplication)
			if err := cassandraCreateTags(params, p); err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, keyspacesService, "CreateKeyspace", params)
			return name, map[string]any{memberArn: out[memberResourceArn], keyspaceName: name}, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			old := props(r.Properties)
			tags, err := cassandraTags(p)
			if err != nil {
				return nil, err
			}

			if p.changed(old, cassandraReplication) {
				spec, ok := p[cassandraReplication]
				if !ok {
					spec = map[string]any{"ReplicationStrategy": "SINGLE_REGION"}
				}

				_, err := c.call(ctx, keyspacesService, "UpdateKeyspace", map[string]any{
					keyspaceName: r.Physical, cassandraReplication: spec,
				})
				if err != nil {
					return nil, err
				}
			}

			return r.Attributes, retagCassandra(ctx, c, old.tags(), tags, r.Attributes[memberArn])
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteCassandra(ctx, c, "DeleteKeyspace", map[string]any{keyspaceName: r.Physical})
		},
	})
	register("AWS::Cassandra::Table", &resourceType{
		name: memberTableName,
		properties: []string{
			keyspaceName, memberTableName, cassandraPartition, cassandraClustering, cassandraRegular,
			propertyBillingMode, cassandraEncryption, cassandraPITR, cassandraTTL, cassandraTimestamps, memberTags,
		},
		replace:    []string{keyspaceName, memberTableName, cassandraPartition, cassandraClustering},
		attributes: []string{memberArn, keyspaceName, memberTableName},
		create:     createCassandraTable, update: updateCassandraTable,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteCassandra(ctx, c, "DeleteTable", cassandraTableTarget(r))
		},
	})
}

func cassandraName(c *client, p props, key string) string {
	if p.has(key) {
		return p.str(key)
	}

	return strings.ReplaceAll(c.generate(48), "-", "_")
}

func cassandraTableTarget(r *resource) map[string]any {
	return map[string]any{keyspaceName: r.Attributes[keyspaceName], memberTableName: r.Physical}
}

func deleteCassandra(ctx context.Context, c *client, op string, params map[string]any) error {
	_, err := c.call(ctx, keyspacesService, op, params)
	if gone(err, "ResourceNotFoundException") {
		return nil
	}

	return err
}

func cassandraTags(p props) (map[string]string, error) {
	list, err := cassandraList(p, memberTags)
	if err != nil {
		return nil, err
	}

	tags := make(map[string]string, len(list))
	for _, raw := range list {
		entry, e := cassandraObject(raw, memberTags, memberKey, memberValue)
		if e != nil {
			return nil, e
		}

		name, named := entry[memberKey].(string)
		value, valued := entry[memberValue].(string)
		if _, dup := tags[name]; dup || !named || !valued || value == "" {
			return nil, validation("Tags require unique string keys and nonempty string values")
		}

		tags[name] = value
	}

	if err = tagging.CheckSet(tags); err != nil {
		return nil, validation("%s", err)
	}

	return tags, nil
}

func cassandraCreateTags(params map[string]any, p props) error {
	tags, err := cassandraTags(p)
	if err != nil {
		return err
	}

	if len(tags) != 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	return nil
}

func retagCassandra(ctx context.Context, c *client, old, next map[string]string, arn any) error {
	set, removed := tagChanges(old, next)
	if len(removed) != 0 {
		tags := make([]any, 0, len(removed))
		for _, key := range removed {
			name := key.(string)
			tags = append(tags, map[string]any{memberKey: name, memberValue: old[name]})
		}

		_, err := c.call(ctx, keyspacesService, opUntagResource, map[string]any{
			memberResourceArn: arn, memberTags: tags,
		})
		if err != nil {
			return err
		}
	}

	if len(set) == 0 {
		return nil
	}

	_, err := c.call(ctx, keyspacesService, opTagResource, map[string]any{
		memberResourceArn: arn, memberTags: tagList(set, memberKey, memberValue),
	})
	return err
}

func createCassandraTable(ctx context.Context, c *client, p props) (
	physical string, attributes map[string]any, err error,
) {
	name := cassandraName(c, p, memberTableName)
	params := pick(map[string]any{memberTableName: name}, p, keyspaceName)
	if tagErr := cassandraCreateTags(params, p); tagErr != nil {
		return "", nil, tagErr
	}
	schema, err := cassandraSchema(p)
	if err != nil {
		return "", nil, err
	}

	params["SchemaDefinition"] = schema
	if optionErr := cassandraOptions(params, nil, p); optionErr != nil {
		return "", nil, optionErr
	}

	out, err := c.call(ctx, keyspacesService, "CreateTable", params)
	return name, map[string]any{
		memberArn:       out[memberResourceArn],
		keyspaceName:    p[keyspaceName],
		memberTableName: name,
	}, err
}

func updateCassandraTable(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	tags, err := cassandraTags(p)
	if err != nil {
		return nil, err
	}

	params := cassandraTableTarget(r)
	if p.changed(old, cassandraRegular) {
		add, err := cassandraAddedColumns(old, p)
		if err != nil {
			return nil, err
		}

		if len(add) > 0 {
			params["AddColumns"] = add
		}
	}

	if err := cassandraOptions(params, old, p); err != nil {
		return nil, err
	}

	if len(params) > 2 {
		if _, err := c.call(ctx, keyspacesService, "UpdateTable", params); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retagCassandra(ctx, c, old.tags(), tags, r.Attributes[memberArn])
}

func cassandraObject(value any, property string, fields ...string) (props, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, validation("%s must be an object", property)
	}

	for field, val := range obj {
		if val == nil || !slices.Contains(fields, field) {
			return nil, validation("Invalid %s member %s", property, field)
		}
	}

	return obj, nil
}

func cassandraList(p props, name string) ([]any, error) {
	if !p.has(name) {
		return nil, nil
	}

	value, ok := p[name].([]any)
	if !ok {
		return nil, validation("%s must be a list", name)
	}

	return value, nil
}

const (
	cassandraColumnName = "ColumnName"
	cassandraColumnType = "ColumnType"
)

func cassandraColumn(raw any) (map[string]any, error) {
	column, err := cassandraObject(raw, "Column", cassandraColumnName, cassandraColumnType)
	if err != nil {
		return nil, err
	}

	return map[string]any{memberName: column[cassandraColumnName], memberType: column[cassandraColumnType]}, nil
}

func cassandraSchema(p props) (map[string]any, error) {
	var columns, partition, clustering []any
	for _, name := range []string{cassandraPartition, cassandraClustering, cassandraRegular} {
		list, err := cassandraList(p, name)
		if err != nil {
			return nil, err
		}

		for _, raw := range list {
			var order any
			if name == cassandraClustering {
				column, e := cassandraObject(raw, name, "Column", "OrderBy")
				if e != nil {
					return nil, e
				}

				raw, order = column["Column"], column["OrderBy"]
				if order == nil {
					order = "ASC"
				}
			}

			column, e := cassandraColumn(raw)
			if e != nil {
				return nil, e
			}

			columns = append(columns, column)
			switch name {
			case cassandraPartition:
				partition = append(partition, map[string]any{memberName: column[memberName]})
			case cassandraClustering:
				clustering = append(clustering, map[string]any{memberName: column[memberName], "OrderBy": order})
			}
		}
	}

	return map[string]any{"AllColumns": columns, "PartitionKeys": partition, "ClusteringKeys": clustering}, nil
}

func cassandraAddedColumns(old, p props) ([]any, error) {
	read := func(p props) ([]any, map[string]string, error) {
		list, err := cassandraList(p, cassandraRegular)
		if err != nil {
			return nil, nil, err
		}

		names := make(map[string]string, len(list))
		columns := make([]any, 0, len(list))
		for _, raw := range list {
			column, e := cassandraColumn(raw)
			if e != nil {
				return nil, nil, e
			}

			name, kind := outText(column, memberName), outText(column, memberType)
			if _, seen := names[name]; seen || name == "" || kind == "" {
				return nil, nil, validation("RegularColumns requires unique names and valid types")
			}

			names[name] = kind
			columns = append(columns, column)
		}

		return columns, names, nil
	}
	_, before, err := read(old)
	if err != nil {
		return nil, err
	}

	columns, after, err := read(p)
	if err != nil {
		return nil, err
	}

	for name, kind := range before {
		if after[name] != kind {
			return nil, validation("Regular column %s cannot be removed or change type", name)
		}
	}

	return slices.DeleteFunc(columns, func(column any) bool {
		_, exists := before[outText(column.(map[string]any), memberName)]
		return exists
	}), nil
}

func cassandraOptions(params map[string]any, old, p props) error {
	if p.changed(old, propertyBillingMode) {
		capacity, err := cassandraCapacity(p)
		if err != nil {
			return err
		}

		params["CapacitySpecification"] = capacity
	}

	if p.changed(old, cassandraEncryption) {
		spec := props{"EncryptionType": "AWS_OWNED_KMS_KEY"}
		if p.has(cassandraEncryption) {
			var err error
			spec, err = cassandraObject(
				p[cassandraEncryption],
				cassandraEncryption,
				"EncryptionType",
				"KmsKeyIdentifier",
			)
			if err != nil {
				return err
			}
		}

		params[cassandraEncryption] = pick(map[string]any{memberType: spec["EncryptionType"]}, spec, "KmsKeyIdentifier")
	}

	if p.changed(old, cassandraTTL) {
		params[cassandraTTL] = float64(0)
		if p.has(cassandraTTL) {
			params[cassandraTTL] = p[cassandraTTL]
		}
	}

	for _, item := range []struct{ property, member string }{
		{cassandraPITR, "PointInTimeRecovery"}, {cassandraTimestamps, "ClientSideTimestamps"},
	} {
		if !p.changed(old, item.property) {
			continue
		}

		status := cassandraDisabled
		if p.has(item.property) {
			switch p.str(item.property) {
			case textTrue:
				status = cassandraEnabled
			case "false":
			default:
				return validation("%s must be a boolean", item.property)
			}
		}

		if item.property == cassandraTimestamps && status == cassandraDisabled {
			if old.truth(item.property) {
				return validation("ClientSideTimestampsEnabled cannot be turned off")
			}

			continue
		}

		params[item.member] = map[string]any{memberStatus: status}
	}

	return nil
}

func cassandraCapacity(p props) (map[string]any, error) {
	if !p.has(propertyBillingMode) {
		return map[string]any{"ThroughputMode": "PAY_PER_REQUEST"}, nil
	}

	billing, err := cassandraObject(p[propertyBillingMode], propertyBillingMode, "Mode", propertyProvisionedThroughput)
	if err != nil {
		return nil, err
	}

	capacity := map[string]any{"ThroughputMode": billing["Mode"]}
	if billing.has(propertyProvisionedThroughput) {
		units, e := cassandraObject(billing[propertyProvisionedThroughput], propertyProvisionedThroughput,
			"ReadCapacityUnits", "WriteCapacityUnits")
		if e != nil {
			return nil, e
		}

		pick(capacity, units, "ReadCapacityUnits", "WriteCapacityUnits")
	}

	return capacity, nil
}
