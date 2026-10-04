package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

// ExecuteStatement runs a PartiQL statement (partiql.go) as the item
// operation it translates to: a SELECT is a Query when its WHERE clause
// fixes the partition key, and otherwise a Scan; INSERT is a PutItem that
// fails on an existing item; UPDATE and DELETE are UpdateItem and DeleteItem
// on the item their WHERE clause names in full.

const maxBatchStatements = 25

// placeholder matches the expression attribute names and values expressions
// use.
var placeholder = regexp.MustCompile(`[#:][A-Za-z0-9_]+`)

// keyAttributes returns the partition and sort key names of a table or one of
// its indexes.
func keyAttributes(d *api.TableDescription, index string) (hash, sort string, err error) {
	schema := d.KeySchema
	if index != "" {
		schema = nil
		for _, g := range d.GlobalSecondaryIndexes {
			if value(g.IndexName) == index {
				schema = g.KeySchema
			}
		}

		for _, l := range d.LocalSecondaryIndexes {
			if value(l.IndexName) == index {
				schema = l.KeySchema
			}
		}

		if schema == nil {
			return "", "", awserror.Validation("The table does not have the specified index: %s", index)
		}
	}

	for _, k := range schema {
		if value(k.KeyType) == "HASH" {
			hash = value(k.AttributeName)
		} else {
			sort = value(k.AttributeName)
		}
	}

	return hash, sort, nil
}

// placeholders returns the names and values that exprs use, as nil maps when
// none are, since DynamoDB rejects unused and empty ones.
func (p *pqParser) placeholdersOf(
	exprs ...string,
) (names map[string]string, values map[string]api.AttributeValue) {
	for _, ph := range placeholder.FindAllString(strings.Join(exprs, " "), -1) {
		if n, ok := p.names[ph]; ok {
			if names == nil {
				names = map[string]string{}
			}

			names[ph] = n
		}

		if v, ok := p.values[ph]; ok {
			if values == nil {
				values = map[string]api.AttributeValue{}
			}

			values[ph] = v
		}
	}

	return names, values
}

func (s *Service) ExecuteStatement(
	ctx context.Context,
	in *api.ExecuteStatementInput,
) (*api.ExecuteStatementOutput, error) {
	st, p, err := parseStatement(value(in.Statement), in.Parameters)
	if err != nil {
		return nil, err
	}

	if st.kind == stmtSelect {
		return s.selectItems(ctx, st, p, in)
	}

	if in.NextToken != nil || in.Limit != nil {
		return nil, awserror.Validation("Limit and NextToken are only valid for SELECT statements")
	}

	attributes, err := s.writeItem(ctx, st, p)
	if err != nil {
		return nil, err
	}

	out := &api.ExecuteStatementOutput{Items: []map[string]api.AttributeValue{}}
	if len(attributes) > 0 {
		out.Items = append(out.Items, attributes)
	}

	return out, nil
}

// describe returns a statement's table description.
func (s *Service) describe(ctx context.Context, name string) (*api.TableDescription, error) {
	var d *api.TableDescription
	err := s.view(ctx, &name, func(t *table) { d = &t.Description })
	return d, err
}

func (s *Service) selectItems(
	ctx context.Context,
	st *statement,
	p *pqParser,
	in *api.ExecuteStatementInput,
) (*api.ExecuteStatementOutput, error) {
	d, err := s.describe(ctx, st.table)
	if err != nil {
		return nil, err
	}

	hash, sort, err := keyAttributes(d, st.index)
	if err != nil {
		return nil, err
	}

	var start map[string]api.AttributeValue
	if in.NextToken != nil {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if decodeErr != nil || json.Unmarshal(raw, &start) != nil {
			return nil, awserror.Validation("Invalid NextToken")
		}
	}

	// A WHERE clause that fixes the partition key makes the SELECT a Query,
	// whose key condition also takes the first sort key condition.
	var conds, keys []*pqCond
	if st.where != nil {
		conds = st.where.conjuncts()
	}

	for i, c := range conds {
		if c.attr == hash && c.eq != nil {
			keys = append(keys, c)
			conds = append(conds[:i:i], conds[i+1:]...)
			break
		}
	}

	if len(keys) > 0 && sort != "" {
		for i, c := range conds {
			if c.attr == sort {
				keys = append(keys, c)
				conds = append(conds[:i:i], conds[i+1:]...)
				break
			}
		}
	}

	projection := strings.Join(st.projection, ", ")
	var items []map[string]api.AttributeValue
	var last map[string]api.AttributeValue
	switch {
	case len(keys) > 0:
		if st.orderBy != "" && st.orderBy != sort {
			return nil, awserror.Validation("ORDER BY must name the sort key")
		}

		keyExpr, filter := join(keys), join(conds)
		names, values := p.placeholdersOf(keyExpr, filter, projection)
		q := &api.QueryInput{
			TableName: &st.table, IndexName: optional(st.index), KeyConditionExpression: &keyExpr,
			FilterExpression: optional(filter), ProjectionExpression: optional(projection),
			ExpressionAttributeNames: names, ExpressionAttributeValues: values, ConsistentRead: in.ConsistentRead,
			Limit: in.Limit, ExclusiveStartKey: start, ScanIndexForward: new(!st.desc),
		}
		out, queryErr := s.Query(ctx, q)
		if queryErr != nil {
			return nil, queryErr
		}

		items, last = value(out.Items), out.LastEvaluatedKey
	case st.orderBy != "":
		return nil, awserror.Validation("ORDER BY needs a WHERE clause that fixes the partition key")
	default:
		filter := ""
		if st.where != nil {
			filter = st.where.render()
		}

		names, values := p.placeholdersOf(filter, projection)
		out, scanErr := s.Scan(ctx, &api.ScanInput{
			TableName: &st.table, IndexName: optional(st.index), FilterExpression: optional(filter),
			ProjectionExpression: optional(projection), ExpressionAttributeNames: names,
			ExpressionAttributeValues: values, ConsistentRead: in.ConsistentRead, Limit: in.Limit,
			ExclusiveStartKey: start,
		})
		if scanErr != nil {
			return nil, scanErr
		}

		items, last = value(out.Items), out.LastEvaluatedKey
	}

	out := &api.ExecuteStatementOutput{Items: items, LastEvaluatedKey: last}
	if out.Items == nil {
		out.Items = []map[string]api.AttributeValue{}
	}

	if last != nil {
		raw, encodeErr := json.Marshal(last)
		if encodeErr != nil {
			return nil, encodeErr
		}

		out.NextToken = new(base64.RawURLEncoding.EncodeToString(raw))
	}

	return out, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// itemKeyOf takes the item key from a WHERE clause's equalities on every key
// attribute, returning it and the other conditions.
func itemKeyOf(d *api.TableDescription, st *statement) (map[string]api.AttributeValue, []*pqCond, error) {
	conds := st.where.conjuncts()
	key := map[string]api.AttributeValue{}
	for _, k := range d.KeySchema {
		name := value(k.AttributeName)
		i := 0
		for i < len(conds) && (conds[i].attr != name || conds[i].eq == nil) {
			i++
		}

		if i == len(conds) {
			return nil, nil, awserror.Validation("Where clause does not contain a mandatory equality on all key " +
				"attributes")
		}

		key[name] = *conds[i].eq
		conds = append(conds[:i:i], conds[i+1:]...)
	}

	return key, conds, nil
}

// writeRequest translates an INSERT, UPDATE, or DELETE to the write it makes:
// INSERT puts an item that must not exist, UPDATE updates an item that must,
// and DELETE deletes one, each with the WHERE clause's other conditions.
func (s *Service) writeRequest(ctx context.Context, st *statement, p *pqParser) (*api.TransactWriteItem, error) {
	d, err := s.describe(ctx, st.table)
	if err != nil {
		return nil, err
	}

	hash, _, err := keyAttributes(d, "")
	if err != nil {
		return nil, err
	}

	if st.kind == stmtInsert {
		condition := fnAttributeNotExists + "(" + p.name(hash) + ")"
		names, _ := p.placeholdersOf(condition)
		return &api.TransactWriteItem{Put: &api.Put{
			TableName: &st.table, Item: *st.item.M, ConditionExpression: &condition, ExpressionAttributeNames: names,
		}}, nil
	}

	key, conds, err := itemKeyOf(d, st)
	if err != nil {
		return nil, err
	}

	if st.kind == stmtDelete {
		condition := join(conds)
		names, values := p.placeholdersOf(condition)
		return &api.TransactWriteItem{Delete: &api.Delete{
			TableName: &st.table, Key: key, ConditionExpression: optional(condition),
			ExpressionAttributeNames: names, ExpressionAttributeValues: values,
		}}, nil
	}

	exists := &pqCond{expr: fnAttributeExists + "(" + p.name(hash) + ")"}
	condition := join(append([]*pqCond{exists}, conds...))
	names, values := p.placeholdersOf(st.update, condition)
	return &api.TransactWriteItem{Update: &api.Update{
		TableName: &st.table, Key: key, UpdateExpression: &st.update, ConditionExpression: &condition,
		ExpressionAttributeNames: names, ExpressionAttributeValues: values,
	}}, nil
}

// writeItem runs an INSERT, UPDATE, or DELETE, returning the attributes its
// RETURNING clause asks for.
func (s *Service) writeItem(ctx context.Context, st *statement, p *pqParser) (map[string]api.AttributeValue, error) {
	w, err := s.writeRequest(ctx, st, p)
	if err != nil {
		return nil, err
	}

	switch {
	case w.Put != nil:
		_, err = s.PutItem(ctx, &api.PutItemInput{
			TableName: w.Put.TableName, Item: w.Put.Item, ConditionExpression: w.Put.ConditionExpression,
			ExpressionAttributeNames: w.Put.ExpressionAttributeNames,
		})
		if isCode(err, "ConditionalCheckFailedException") {
			return nil, awserror.New("DuplicateItemException", "Duplicate primary key exists in table")
		}

		return nil, err
	case w.Delete != nil:
		out, deleteErr := s.DeleteItem(ctx, &api.DeleteItemInput{
			TableName: w.Delete.TableName, Key: w.Delete.Key, ConditionExpression: w.Delete.ConditionExpression,
			ExpressionAttributeNames:  w.Delete.ExpressionAttributeNames,
			ExpressionAttributeValues: w.Delete.ExpressionAttributeValues, ReturnValues: optional(st.returning),
		})
		if deleteErr != nil {
			return nil, deleteErr
		}

		return out.Attributes, nil
	}

	out, err := s.UpdateItem(ctx, &api.UpdateItemInput{
		TableName: w.Update.TableName, Key: w.Update.Key, UpdateExpression: w.Update.UpdateExpression,
		ConditionExpression: w.Update.ConditionExpression, ExpressionAttributeNames: w.Update.ExpressionAttributeNames,
		ExpressionAttributeValues: w.Update.ExpressionAttributeValues, ReturnValues: optional(st.returning),
	})
	if err != nil {
		return nil, err
	}

	return out.Attributes, nil
}

func isCode(err error, code string) bool {
	var awsErr *awserror.Error
	return errors.As(err, &awsErr) && awsErr.Code == code
}

// BatchExecuteStatement runs up to 25 statements, all reads or all writes,
// each on its own: a read must name one item by its full key, and a
// statement that fails reports its error in its response.
func (s *Service) BatchExecuteStatement(
	ctx context.Context,
	in *api.BatchExecuteStatementInput,
) (*api.BatchExecuteStatementOutput, error) {
	if len(in.Statements) == 0 || len(in.Statements) > maxBatchStatements {
		return nil, awserror.Validation("Statements must hold 1 to %d statements", maxBatchStatements)
	}

	// Every statement is parsed before any runs, so a batch that mixes reads
	// and writes changes nothing.
	type parsed struct {
		st  *statement
		p   *pqParser
		err error
	}

	statements := make([]parsed, len(in.Statements))
	reads := 0
	for i := range in.Statements {
		st, p, err := parseStatement(value(in.Statements[i].Statement), in.Statements[i].Parameters)
		statements[i] = parsed{st: st, p: p, err: err}
		if err == nil && st.kind == stmtSelect {
			reads++
		}
	}

	if reads != 0 && reads != len(in.Statements) {
		return nil, awserror.Validation("Statements in a batch must be all reads or all writes")
	}

	out := &api.BatchExecuteStatementOutput{Responses: make([]api.BatchStatementResponse, len(in.Statements))}
	for i, ps := range statements {
		err := ps.err
		if err == nil {
			out.Responses[i].TableName = &ps.st.table
			err = s.batchStatement(ctx, ps.st, ps.p, &in.Statements[i], &out.Responses[i])
		}

		if err != nil {
			code, message := "InternalServerError", err.Error()
			var awsErr *awserror.Error
			if errors.As(err, &awsErr) {
				code, message = batchErrorCode(awsErr.Code), awsErr.Message
			}

			out.Responses[i].Error = &api.BatchStatementError{Code: &code, Message: &message}
		}
	}

	return out, nil
}

func (s *Service) batchStatement(
	ctx context.Context,
	st *statement,
	p *pqParser,
	r *api.BatchStatementRequest,
	response *api.BatchStatementResponse,
) error {
	if st.kind != stmtSelect {
		_, err := s.writeItem(ctx, st, p)
		return err
	}

	d, err := s.describe(ctx, st.table)
	if err != nil {
		return err
	}

	if st.where == nil || st.index != "" {
		return awserror.Validation("A batch read must name one item of a table by its full key")
	}

	if _, _, err = itemKeyOf(d, st); err != nil {
		return err
	}

	got, err := s.selectItems(ctx, st, p, &api.ExecuteStatementInput{ConsistentRead: r.ConsistentRead})
	if err != nil {
		return err
	}

	if len(got.Items) > 0 {
		response.Item = maps.Clone(got.Items[0])
	}

	return nil
}

// batchErrorCode maps an error code to BatchStatementError's.
func batchErrorCode(code string) string {
	switch code {
	case "ValidationException":
		return "ValidationError"
	case "InternalServerError", "RequestLimitExceeded":
		return code
	}

	return strings.TrimSuffix(code, "Exception")
}

// maxTransactStatements is how many statements ExecuteTransaction takes.
const maxTransactStatements = 100

// ExecuteTransaction runs up to 100 statements as one transaction, all reads
// (TransactGetItems, each naming one item by its full key) or all writes
// (TransactWriteItems, where an INSERT of an existing item is cancelled with
// the DuplicateItem reason).
func (s *Service) ExecuteTransaction(
	ctx context.Context,
	in *api.ExecuteTransactionInput,
) (*api.ExecuteTransactionOutput, error) {
	n := len(in.TransactStatements)
	if n == 0 || n > maxTransactStatements {
		return nil, awserror.Validation("TransactStatements must hold 1 to %d statements", maxTransactStatements)
	}

	statements := make([]*statement, n)
	parsers := make([]*pqParser, n)
	reads := 0
	for i := range in.TransactStatements {
		st, p, err := parseStatement(value(in.TransactStatements[i].Statement), in.TransactStatements[i].Parameters)
		if err != nil {
			return nil, err
		}

		if st.returning != "" {
			return nil, awserror.Validation("RETURNING is not supported in a transaction")
		}

		statements[i], parsers[i] = st, p
		if st.kind == stmtSelect {
			reads++
		}
	}

	// SDKs send a ClientRequestToken with every transaction; reads ignore it.
	switch {
	case reads == n:
		return s.readTransaction(ctx, statements, parsers)
	case reads != 0:
		return nil, awserror.Validation("Statements in a transaction must be all reads or all writes")
	}

	items := make([]api.TransactWriteItem, n)
	for i, st := range statements {
		w, err := s.writeRequest(ctx, st, parsers[i])
		if err != nil {
			return nil, err
		}

		onFailure := in.TransactStatements[i].ReturnValuesOnConditionCheckFailure
		switch {
		case w.Put != nil:
			w.Put.ReturnValuesOnConditionCheckFailure = onFailure
		case w.Delete != nil:
			w.Delete.ReturnValuesOnConditionCheckFailure = onFailure
		default:
			w.Update.ReturnValuesOnConditionCheckFailure = onFailure
		}

		items[i] = *w
	}

	_, err := s.TransactWriteItems(ctx, &api.TransactWriteItemsInput{
		TransactItems: items, ClientRequestToken: in.ClientRequestToken,
	})
	if err != nil {
		return nil, duplicates(err, statements)
	}

	return &api.ExecuteTransactionOutput{Responses: []api.ItemResponse{}}, nil
}

// readTransaction reads the items SELECT statements name in one transaction.
func (s *Service) readTransaction(
	ctx context.Context,
	statements []*statement,
	parsers []*pqParser,
) (*api.ExecuteTransactionOutput, error) {
	gets := make([]api.TransactGetItem, len(statements))
	for i, st := range statements {
		d, err := s.describe(ctx, st.table)
		if err != nil {
			return nil, err
		}

		if st.where == nil || st.index != "" {
			return nil, awserror.Validation("A transaction read must name one item of a table by its full key")
		}

		key, conds, err := itemKeyOf(d, st)
		if err != nil {
			return nil, err
		}

		if len(conds) > 0 {
			return nil, awserror.Validation("A transaction read can only name an item by its key")
		}

		projection := strings.Join(st.projection, ", ")
		names, _ := parsers[i].placeholdersOf(projection)
		gets[i] = api.TransactGetItem{Get: &api.Get{
			TableName: &st.table, Key: key, ProjectionExpression: optional(projection), ExpressionAttributeNames: names,
		}}
	}

	out, err := s.TransactGetItems(ctx, &api.TransactGetItemsInput{TransactItems: gets})
	if err != nil {
		return nil, err
	}

	return &api.ExecuteTransactionOutput{Responses: out.Responses}, nil
}

// duplicates reports a cancelled INSERT of an existing item with the
// DuplicateItem reason, as ExecuteTransaction does.
func duplicates(err error, statements []*statement) error {
	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "TransactionCanceledException" {
		return err
	}

	reasons, _ := awsErr.Fields["CancellationReasons"].([]map[string]any)
	codes := make([]string, len(reasons))
	for i, reason := range reasons {
		if statements[i].kind == stmtInsert && reason[reasonCode] == "ConditionalCheckFailed" {
			reason[reasonCode], reason["Message"] = "DuplicateItem", "Duplicate primary key exists in table"
			delete(reason, "Item")
		}

		codes[i], _ = reason[reasonCode].(string)
	}

	awsErr.Message = "Transaction cancelled, please refer cancellation reasons for specific reasons [" +
		strings.Join(codes, ", ") + "]"
	return awsErr
}
