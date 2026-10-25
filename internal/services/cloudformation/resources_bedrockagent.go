package cloudformation

import (
	"context"
	"maps"
	"reflect"
)

const bedrockAgentService = "bedrockagent"

func bedrockAgentTagger(arn string) *tagger {
	return &tagger{
		service: bedrockAgentService,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: arn},
	}
}

func bedrockAgentParams(p props, fields []string) map[string]any {
	params := pick(map[string]any{}, p, fields...)
	delete(params, memberTags)
	return params
}

// bedrockStable observes and then completes the deterministic intermediate state.
func bedrockStable(ctx context.Context, c *client, kind string, params map[string]any) (map[string]any, error) {
	var value map[string]any
	for range 2 {
		out, err := c.call(ctx, bedrockAgentService, "Get"+kind, params)
		if err != nil {
			return nil, err
		}

		value, _ = out[kind].(map[string]any)
	}

	return value, nil
}

func init() {
	agentFields := []string{
		"AgentName",
		"AgentResourceRoleArn",
		"FoundationModel",
		"Instruction",
		memberDescription,
		"IdleSessionTTLInSeconds",
		"CustomerEncryptionKeyArn",
		"PromptOverrideConfiguration",
		"GuardrailConfiguration",
		"MemoryConfiguration",
		"AgentCollaboration",
		memberTags,
		bedrockActionGroups,
		bedrockKnowledgeBases,
		bedrockAutoPrepare,
		bedrockSkipResourceInUseCheckOnDelete,
		bedrockTestAliasTags,
	}

	register("AWS::Bedrock::Agent", &resourceType{
		properties: agentFields,
		attributes: []string{
			bedrockAgentId,
			"AgentArn",
			"AgentStatus",
			bedrockAgentVersion,
			bedrockCreatedAt,
			bedrockUpdatedAt,
			"PreparedAt",
			bedrockFailureReasons,
			"RecommendedActions",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := bedrockAgentParams(p, agentFields)
			for _, field := range []string{
				bedrockActionGroups,
				bedrockKnowledgeBases,
				bedrockAutoPrepare,
				bedrockSkipResourceInUseCheckOnDelete,
				bedrockTestAliasTags,
			} {
				delete(params, field)
			}

			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, bedrockAgentService, "CreateAgent", params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out[bedrockAgent].(map[string]any)
			id := outText(value, bedrockAgentId)
			if _, checkErr1 := bedrockStable(
				ctx,
				c,
				bedrockAgent,
				map[string]any{bedrockAgentId: id},
			); checkErr1 != nil {
				return "", nil, checkErr1
			}

			if checkErr2 := bedrockAgentChildren(ctx, c, id, nil, p); checkErr2 != nil {
				return "", nil, checkErr2
			}

			value, err = bedrockAgentPrepare(ctx, c, id, p)
			return id, value, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := bedrockAgentParams(p, agentFields)
			for _, field := range []string{
				bedrockActionGroups,
				bedrockKnowledgeBases,
				bedrockAutoPrepare,
				bedrockSkipResourceInUseCheckOnDelete,
				bedrockTestAliasTags,
			} {
				delete(params, field)
			}

			params[bedrockAgentId] = r.Physical
			if _, checkErr3 := c.call(ctx, bedrockAgentService, "UpdateAgent", params); checkErr3 != nil {
				return nil, checkErr3
			}

			if _, checkErr4 := bedrockStable(
				ctx,
				c,
				bedrockAgent,
				map[string]any{bedrockAgentId: r.Physical},
			); checkErr4 != nil {
				return nil, checkErr4
			}

			if checkErr5 := bedrockAgentChildren(ctx, c, r.Physical, props(r.Properties), p); checkErr5 != nil {
				return nil, checkErr5
			}

			if checkErr6 := retag(
				ctx,
				c,
				props(r.Properties),
				p,
				bedrockAgentTagger(outText(r.Attributes, "AgentArn")),
			); checkErr6 != nil {
				return nil, checkErr6
			}

			return bedrockAgentPrepare(ctx, c, r.Physical, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := map[string]any{bedrockAgentId: r.Physical}

			if value, ok := r.Properties[bedrockSkipResourceInUseCheckOnDelete]; ok {
				params["SkipResourceInUseCheck"] = value
			}

			if _, checkErr7 := c.call(ctx, bedrockAgentService, "DeleteAgent", params); checkErr7 != nil {
				return checkErr7
			}

			_, err := c.call(ctx, bedrockAgentService, "GetAgent", map[string]any{bedrockAgentId: r.Physical})
			return err
		},
	})
	for _, d := range []struct {
		kind, id, arn          string
		fields, replace, attrs []string
	}{
		{
			bedrockAgentAlias,
			"AgentAliasId",
			"AgentAliasArn",
			[]string{
				bedrockAgentId,
				"AgentAliasName",
				memberDescription,
				"RoutingConfiguration",
				memberTags,
			},
			[]string{bedrockAgentId},
			[]string{
				"AgentAliasStatus",
				"AgentAliasHistoryEvents",
				bedrockCreatedAt,
				bedrockUpdatedAt,
			},
		},
		{
			"KnowledgeBase",
			bedrockKnowledgeBaseId,
			"KnowledgeBaseArn",
			[]string{
				memberName,
				memberDescription,
				memberRoleArn,
				"KnowledgeBaseConfiguration",
				"StorageConfiguration",
				memberTags,
			},
			[]string{"StorageConfiguration"},
			[]string{
				memberStatus,
				bedrockCreatedAt,
				bedrockUpdatedAt,
				bedrockFailureReasons,
			},
		},
		{
			bedrockDataSource,
			"DataSourceId",
			"",
			[]string{
				bedrockKnowledgeBaseId,
				memberName,
				memberDescription,
				"DataSourceConfiguration",
				"DataDeletionPolicy",
				"ServerSideEncryptionConfiguration",
				"VectorIngestionConfiguration",
			},
			[]string{bedrockKnowledgeBaseId},
			[]string{
				bedrockDataSourceStatus,
				bedrockCreatedAt,
				bedrockUpdatedAt,
				bedrockFailureReasons,
			},
		},
	} {
		attrs := append([]string{d.id}, d.attrs...)
		if d.arn != "" {
			attrs = append(attrs, d.arn)
		}

		register("AWS::Bedrock::"+d.kind, &resourceType{
			properties: d.fields, attributes: attrs, replace: d.replace,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := bedrockAgentParams(p, d.fields)
				if d.arn != "" {
					params[memberTags] = tagMap(p.tags())
				}

				out, err := c.call(ctx, bedrockAgentService, "Create"+d.kind, params)
				if err != nil {
					return "", nil, err
				}

				value, _ := out[d.kind].(map[string]any)
				id := outText(value, d.id)
				switch d.kind {
				case bedrockAgentAlias:
					value, err = bedrockStable(
						ctx,
						c,
						d.kind,
						map[string]any{bedrockAgentId: p.str(bedrockAgentId), d.id: id},
					)
				case bedrockDataSource:
					value[bedrockDataSourceStatus] = value[memberStatus]
				}

				physical := id
				if d.kind == bedrockAgentAlias {
					physical = p.str(bedrockAgentId) + "|" + id
				}

				if d.kind == bedrockDataSource {
					physical = p.str(bedrockKnowledgeBaseId) + "|" + id
				}

				return physical, value, err
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				params := bedrockAgentParams(p, d.fields)
				params[d.id] = outText(r.Attributes, d.id)
				if _, checkErr8 := c.call(ctx, bedrockAgentService, "Update"+d.kind, params); checkErr8 != nil {
					return nil, checkErr8
				}

				if d.arn != "" {
					if checkErr9 := retag(
						ctx,
						c,
						props(r.Properties),
						p,
						bedrockAgentTagger(outText(r.Attributes, d.arn)),
					); checkErr9 != nil {
						return nil, checkErr9
					}
				}

				query := map[string]any{d.id: params[d.id]}

				if d.kind == bedrockAgentAlias {
					query[bedrockAgentId] = p.str(bedrockAgentId)
				}

				if d.kind == bedrockDataSource {
					query[bedrockKnowledgeBaseId] = p.str(bedrockKnowledgeBaseId)
				}

				value, err := bedrockStable(ctx, c, d.kind, query)
				if d.kind == bedrockDataSource && value != nil {
					value[bedrockDataSourceStatus] = value[memberStatus]
				}

				return value, err
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				params := map[string]any{d.id: outText(r.Attributes, d.id)}

				if d.kind == bedrockAgentAlias {
					params[bedrockAgentId] = props(r.Properties).str(bedrockAgentId)
				}

				if d.kind == bedrockDataSource {
					params[bedrockKnowledgeBaseId] = props(r.Properties).str(bedrockKnowledgeBaseId)
				}

				_, err := c.call(ctx, bedrockAgentService, "Delete"+d.kind, params)
				return err
			},
		})
	}
}

func bedrockAgentPrepare(ctx context.Context, c *client, id string, p props) (map[string]any, error) {
	if enabled, _ := p[bedrockAutoPrepare].(bool); enabled {
		if _, checkErr10 := c.call(
			ctx,
			bedrockAgentService,
			"PrepareAgent",
			map[string]any{bedrockAgentId: id},
		); checkErr10 != nil {
			return nil, checkErr10
		}
	}

	return bedrockStable(ctx, c, bedrockAgent, map[string]any{bedrockAgentId: id})
}

func bedrockAgentChildren(ctx context.Context, c *client, id string, old, p props) error {
	for _, d := range []struct{ property, kind, list, summary, id, name string }{
		{
			bedrockActionGroups,
			"AgentActionGroup",
			"ListAgentActionGroups",
			"ActionGroupSummaries",
			"ActionGroupId",
			"ActionGroupName",
		},
		{
			bedrockKnowledgeBases,
			"AgentKnowledgeBase",
			"ListAgentKnowledgeBases",
			"AgentKnowledgeBaseSummaries",
			bedrockKnowledgeBaseId,
			bedrockKnowledgeBaseId,
		},
	} {
		out, err := c.call(
			ctx,
			bedrockAgentService,
			d.list,
			map[string]any{bedrockAgentId: id, bedrockAgentVersion: bedrockDRAFT},
		)
		if err != nil {
			return err
		}

		existing := map[string]string{}

		rows, _ := out[d.summary].([]any)
		for _, row := range rows {
			value, _ := row.(map[string]any)
			existing[outText(value, d.name)] = outText(value, d.id)
		}

		desired, _ := p[d.property].([]any)
		for _, row := range desired {
			value, _ := row.(map[string]any)
			params := maps.Clone(value)
			delete(params, bedrockSkipResourceInUseCheckOnDelete)
			params[bedrockAgentId] = id
			params[bedrockAgentVersion] = bedrockDRAFT
			name := outText(value, d.name)
			operation := "Create" + d.kind
			if d.kind == "AgentKnowledgeBase" {
				operation = "AssociateAgentKnowledgeBase"
			}

			if childID, ok := existing[name]; ok {
				operation = "Update" + d.kind
				params[d.id] = childID
			}

			if _, checkErr11 := c.call(ctx, bedrockAgentService, operation, params); checkErr11 != nil {
				return checkErr11
			}

			delete(existing, name)
		}

		for _, childID := range existing {
			params := map[string]any{bedrockAgentId: id, bedrockAgentVersion: bedrockDRAFT, d.id: childID}

			operation := "Delete" + d.kind
			if d.kind == "AgentActionGroup" {
				params["SkipResourceInUseCheck"] = true
			} else {
				operation = "DisassociateAgentKnowledgeBase"
			}

			if _, checkErr12 := c.call(ctx, bedrockAgentService, operation, params); checkErr12 != nil {
				return checkErr12
			}
		}
	}

	before := props{}

	if old != nil {
		if value, ok := old[bedrockTestAliasTags]; ok {
			before[memberTags] = value
		}
	}

	after := props{memberTags: p[bedrockTestAliasTags]}
	// The built-in test alias uses the same resource tagging operations.
	value, err := c.call(
		ctx,
		bedrockAgentService,
		"GetAgentAlias",
		map[string]any{bedrockAgentId: id, "AgentAliasId": "TSTALIASID"},
	)
	if err != nil {
		return err
	}

	alias, _ := value[bedrockAgentAlias].(map[string]any)
	return retag(ctx, c, before, after, bedrockAgentTagger(outText(alias, "AgentAliasArn")))
}

// bedrockDataSourceReplaces follows immutable nested properties in the CloudFormation specification.
func bedrockDataSourceReplaces(old, p props) bool {
	if p.changed(old, bedrockKnowledgeBaseId) {
		return true
	}

	for _, entry := range []struct {
		property string
		fields   []string
	}{
		{"DataSourceConfiguration", []string{memberType}},
		{"VectorIngestionConfiguration", []string{"ChunkingConfiguration", "ParsingConfiguration"}},
	} {
		before, _ := old[entry.property].(map[string]any)
		after, _ := p[entry.property].(map[string]any)
		for _, field := range entry.fields {
			if !reflect.DeepEqual(before[field], after[field]) {
				return true
			}
		}
	}

	return false
}

const (
	bedrockActionGroups                   = "ActionGroups"
	bedrockKnowledgeBases                 = "KnowledgeBases"
	bedrockAutoPrepare                    = "AutoPrepare"
	bedrockSkipResourceInUseCheckOnDelete = "SkipResourceInUseCheckOnDelete"
	bedrockTestAliasTags                  = "TestAliasTags"
	bedrockAgentId                        = "AgentId"
	bedrockAgentVersion                   = "AgentVersion"
	bedrockCreatedAt                      = "CreatedAt"
	bedrockUpdatedAt                      = "UpdatedAt"
	bedrockFailureReasons                 = "FailureReasons"
	bedrockAgent                          = "Agent"
	bedrockAgentAlias                     = "AgentAlias"
	bedrockKnowledgeBaseId                = "KnowledgeBaseId"
	bedrockDataSource                     = "DataSource"
	bedrockDataSourceStatus               = "DataSourceStatus"
	bedrockDRAFT                          = "DRAFT"
)
