package dynamodb

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type requiredAction struct{ action, index string }

func (s *Service) AuthorizationResources(
	ctx context.Context,
	_ string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	names := catalog.NestedInputStrings(input, "TableName")
	names = append(names, catalog.InputMapKeys(input, "RequestItems")...)
	for _, field := range []string{
		"TableArn", "ResourceArn", "SourceTableName", "TargetTableName", "BackupArn", "ExportArn",
	} {
		if ref := catalog.InputString(input, field); ref != "" {
			names = append(names, ref)
		}
	}

	// Per-item actions for transactions and PartiQL come from the SAR API table:
	// https://docs.aws.amazon.com/service-authorization/latest/reference/list_dynamodb.html
	actions := map[string][]requiredAction{}
	switch in := input.(type) {
	case *api.TransactGetItemsInput:
		for _, item := range in.TransactItems {
			if item.Get != nil {
				actions[common.Value(item.Get.TableName)] = []requiredAction{{action: "dynamodb:GetItem"}}
			}
		}
	case *api.TransactWriteItemsInput:
		for _, item := range in.TransactItems {
			for _, check := range []struct {
				input  any
				action string
			}{
				{item.Put, "PutItem"},
				{item.Delete, "DeleteItem"},
				{item.Update, "UpdateItem"},
				{item.ConditionCheck, "ConditionCheckItem"},
			} {
				if table := catalog.InputString(check.input, "TableName"); table != "" {
					actions[table] = append(actions[table], requiredAction{action: "dynamodb:" + check.action})
				}
			}
		}
	case *api.ExecuteStatementInput:
		if targetErr := statementTarget(common.Value(in.Statement), in.Parameters, actions); targetErr != nil {
			return nil, targetErr
		}
	case *api.BatchExecuteStatementInput:
		for _, item := range in.Statements {
			if targetErr := statementTarget(
				common.Value(item.Statement),
				item.Parameters,
				actions,
			); targetErr != nil {
				return nil, targetErr
			}
		}
	case *api.ExecuteTransactionInput:
		for _, item := range in.TransactStatements {
			if targetErr := statementTarget(
				common.Value(item.Statement),
				item.Parameters,
				actions,
			); targetErr != nil {
				return nil, targetErr
			}
		}
	}

	for name := range actions {
		names = append(names, name)
	}

	slices.Sort(names)
	names = slices.Compact(names)
	var out []catalog.AuthorizationResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range names {
			if name == "" {
				continue
			}

			arn := name
			if !strings.HasPrefix(arn, "arn:") {
				arn = "arn:aws:dynamodb:" + m.Region + ":" + m.AccountID + ":table/" + name
			}

			_, tableName, _ := strings.Cut(arn, ":table/")
			tableName, _, _ = strings.Cut(tableName, "/")
			target := catalog.AuthorizationResource{ARN: arn, TagsKnown: true}
			t, _, readErr := storage.Read[table](tx, m.AccountID+"/"+m.Region+"/dynamodb/"+tableName)
			if readErr != nil {
				return readErr
			}

			if t != nil {
				target.Tags = common.CloneTags(t.Tags)
			}

			required := actions[name]
			if len(required) == 0 {
				required = []requiredAction{{}}
			}

			for _, required := range required {
				index := catalog.InputString(input, "IndexName")
				if index == "" {
					index = required.index
				}

				target.Action = required.action
				out = append(out, target)
				if index != "" {
					out = append(
						out,
						catalog.AuthorizationResource{
							ARN:       arn + "/index/" + index,
							Action:    required.action,
							Tags:      target.Tags,
							TagsKnown: true,
						},
					)
				}
			}
		}

		return nil
	})
	return out, err
}

func statementTarget(
	text string,
	params []api.AttributeValue,
	actions map[string][]requiredAction,
) error {
	st, _, err := parseStatement(text, params)
	if err != nil {
		return err
	}

	suffix := ""
	switch st.kind {
	case stmtSelect:
		suffix = "Select"
	case stmtInsert:
		suffix = "Insert"
	case stmtUpdate:
		suffix = "Update"
	case stmtDelete:
		suffix = "Delete"
	}

	actions[st.table] = append(actions[st.table], requiredAction{action: "dynamodb:PartiQL" + suffix, index: st.index})
	return nil
}
