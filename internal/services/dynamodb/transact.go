package dynamodb

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxTransactItems = 100
	reasonCode       = "Code"
	// tokenWindow is how long a ClientRequestToken makes TransactWriteItems idempotent.
	tokenWindow = 10 * time.Minute
)

func multipleOperations() error {
	return awserror.Validation("Transaction request cannot include multiple operations on one item")
}

// TransactGetItems reads up to 100 items from one snapshot.
func (s *Service) TransactGetItems(
	ctx context.Context,
	in *api.TransactGetItemsInput,
) (*api.TransactGetItemsOutput, error) {
	if len(in.TransactItems) == 0 || len(in.TransactItems) > maxTransactItems {
		return nil, awserror.Validation("TransactItems must hold 1 to 100 items")
	}

	keys := make([]string, len(in.TransactItems))
	projections := make([]projection, len(in.TransactItems))
	for i, ti := range in.TransactItems {
		if ti.Get == nil {
			return nil, awserror.Validation("Each TransactGetItem must hold a Get")
		}

		key, err := tableKey(ctx, ti.Get.TableName)
		if err != nil {
			return nil, err
		}

		ex, err := newExpressions(ti.Get.ExpressionAttributeNames, nil)
		if err != nil {
			return nil, err
		}

		if projections[i], err = ex.projection(ti.Get.ProjectionExpression); err != nil {
			return nil, err
		}

		if guardErr := ex.finish(); guardErr != nil {
			return nil, guardErr
		}

		keys[i] = key
	}

	out := &api.TransactGetItemsOutput{Responses: make([]api.ItemResponse, len(in.TransactItems))}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		tables := map[string]*table{}
		seen := map[string]bool{}
		for i, ti := range in.TransactItems {
			t, err := cachedTable(tx, tables, keys[i])
			if err != nil {
				return err
			}

			record, it, _, err := target(tx, keys[i], t, ti.Get.Key, true)
			if err != nil {
				return err
			}

			if seen[record] {
				return multipleOperations()
			}

			seen[record] = true
			out.Responses[i].Item = projections[i].apply(it)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func cachedTable(tx storage.ReadTx, tables map[string]*table, key string) (*table, error) {
	if t, ok := tables[key]; ok {
		return t, nil
	}

	t, err := load(tx, key)
	if err != nil {
		return nil, err
	}

	tables[key] = t
	return t, nil
}

// transactWrite is one parsed TransactWriteItem: a condition check, or a put,
// delete, or update of the item keyed by key.
type transactWrite struct {
	req       *writeRequest
	key       item
	put       item
	upd       *update
	delete    bool
	returnOld bool
}

func newTransactWrite(ctx context.Context, ti *api.TransactWriteItem) (*transactWrite, error) {
	var (
		w                        = &transactWrite{}
		name, cond, returnValues *string
		names                    map[string]string
		values                   map[string]api.AttributeValue
		updateExpression         *string
		set                      int
	)

	if c := ti.ConditionCheck; c != nil {
		if c.ConditionExpression == nil {
			return nil, awserror.Validation("ConditionCheck requires a ConditionExpression")
		}

		set++
		w.key, name, cond, names, values, returnValues = c.Key, c.TableName, c.ConditionExpression,
			c.ExpressionAttributeNames, c.ExpressionAttributeValues, c.ReturnValuesOnConditionCheckFailure
	}

	if p := ti.Put; p != nil {
		set++
		w.key, w.put, name, cond, names, values, returnValues = p.Item, p.Item, p.TableName, p.ConditionExpression,
			p.ExpressionAttributeNames, p.ExpressionAttributeValues, p.ReturnValuesOnConditionCheckFailure
	}

	if d := ti.Delete; d != nil {
		set++
		w.delete = true
		w.key, name, cond, names, values, returnValues = d.Key, d.TableName, d.ConditionExpression,
			d.ExpressionAttributeNames, d.ExpressionAttributeValues, d.ReturnValuesOnConditionCheckFailure
	}

	if u := ti.Update; u != nil {
		set++
		updateExpression = u.UpdateExpression
		w.key, name, cond, names, values, returnValues = u.Key, u.TableName, u.ConditionExpression,
			u.ExpressionAttributeNames, u.ExpressionAttributeValues, u.ReturnValuesOnConditionCheckFailure
	}

	if set != 1 || w.key == nil {
		return nil, awserror.Validation("Each TransactWriteItem must hold exactly one of ConditionCheck, " +
			"Put, Delete, or Update")
	}

	switch value(returnValues) {
	case "", "NONE":
	case returnAllOld:
		w.returnOld = true
	default:
		return nil, awserror.Validation("ReturnValuesOnConditionCheckFailure must be NONE or ALL_OLD")
	}

	req, err := newWriteRequest(ctx, name, cond, names, values)
	if err != nil {
		return nil, err
	}

	if ti.Update != nil {
		if w.upd, err = req.ex.update(updateExpression); err != nil {
			return nil, err
		}
	}

	if guardErr := req.ex.finish(); guardErr != nil {
		return nil, guardErr
	}

	w.req = req
	return w, nil
}

// tokenRecord remembers a TransactWriteItems ClientRequestToken: the request
// it made idempotent and when it stops doing so.
type tokenRecord struct {
	Expires time.Time
	Request string
}

// TransactWriteItems evaluates every condition against one snapshot, then
// applies all writes or none. A ClientRequestToken repeated within ten minutes
// with the same items succeeds without writing again.
func (s *Service) TransactWriteItems(
	ctx context.Context,
	in *api.TransactWriteItemsInput,
) (*api.TransactWriteItemsOutput, error) {
	if len(in.TransactItems) == 0 || len(in.TransactItems) > maxTransactItems {
		return nil, awserror.Validation("TransactItems must hold 1 to 100 items")
	}

	if in.ClientRequestToken != nil && (*in.ClientRequestToken == "" || len(*in.ClientRequestToken) > 36) {
		return nil, awserror.Validation("ClientRequestToken must be 1 to 36 characters")
	}

	writes := make([]*transactWrite, len(in.TransactItems))
	for i := range in.TransactItems {
		w, err := newTransactWrite(ctx, &in.TransactItems[i])
		if err != nil {
			return nil, err
		}

		writes[i] = w
	}

	encoded, err := json.Marshal(in.TransactItems)
	if err != nil {
		return nil, fmt.Errorf("encode transaction: %w", err)
	}

	digest := sha256.Sum256(encoded)
	m, _ := request.FromContext(ctx)
	tokens := m.AccountID + "/" + m.Region + "/dynamodb-token/"
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		now := time.Now()
		if in.ClientRequestToken != nil {
			done, tokenErr := claimToken(tx, tokens, *in.ClientRequestToken, hex.EncodeToString(digest[:]), now)
			if done || tokenErr != nil {
				return tokenErr
			}
		}

		return transact(tx, writes)
	})
	if err != nil {
		return nil, err
	}

	return &api.TransactWriteItemsOutput{}, nil
}

// claimToken purges expired tokens and records token for the request whose
// digest is given. It reports done when the token already committed it.
func claimToken(tx storage.WriteTx, prefix, token, digest string, now time.Time) (done bool, err error) {
	for _, key := range tx.Keys(prefix) {
		var r tokenRecord
		data, _ := tx.Get(key)
		if guardErr := json.Unmarshal(data, &r); guardErr != nil {
			return false, fmt.Errorf("decode token: %w", guardErr)
		}

		if !now.Before(r.Expires) {
			tx.Delete(key)
		}
	}

	key := prefix + base64.RawURLEncoding.EncodeToString([]byte(token))
	if data, ok := tx.Get(key); ok {
		var r tokenRecord
		if guardErr := json.Unmarshal(data, &r); guardErr != nil {
			return false, fmt.Errorf("decode token: %w", guardErr)
		}

		if r.Request != digest {
			return false, awserror.New("IdempotentParameterMismatchException",
				"The ClientRequestToken was used with a different request within the last ten minutes")
		}

		return true, nil
	}

	data, err := json.Marshal(tokenRecord{Expires: now.Add(tokenWindow), Request: digest})
	if err != nil {
		return false, fmt.Errorf("encode token: %w", err)
	}

	tx.Put(key, data)
	return false, nil
}

// transact checks every write against the items as they are before the
// transaction, then applies them all, or returns TransactionCanceledException
// with a reason for each write.
func transact(tx storage.WriteTx, writes []*transactWrite) error {
	type staged struct {
		t      *table
		old    item
		next   item
		record string
		found  bool
	}

	tables := map[string]*table{}
	seen := map[string]bool{}
	stages := make([]staged, len(writes))
	reasons := make([]map[string]any, len(writes))
	codes := make([]string, len(writes))
	canceled := false
	for i, w := range writes {
		t, err := cachedTable(tx, tables, w.req.key)
		if err != nil {
			return err
		}

		if w.upd != nil {
			if guardErr := updatesKey(t, w.upd); guardErr != nil {
				return guardErr
			}
		}

		record, old, found, err := target(tx, w.req.key, t, w.key, w.put == nil)
		if err != nil {
			return err
		}

		if seen[record] {
			return multipleOperations()
		}

		seen[record] = true
		stages[i] = staged{t: t, record: record, old: old, found: found}
		reason := map[string]any{reasonCode: "None"}
		switch {
		case !w.req.cond.match(old):
			reason = map[string]any{reasonCode: "ConditionalCheckFailed", "Message": "The conditional request failed"}
			if w.returnOld && found {
				reason["Item"] = old
			}
		case w.put != nil:
			err = checkItem(t, w.put, "Item size has exceeded the maximum allowed size")
			stages[i].next = w.put
		case w.upd != nil:
			stages[i].next, err = updated(t, w.upd, w.key, old, found)
		}

		if err != nil {
			var awsErr *awserror.Error
			if !errors.As(err, &awsErr) {
				return err
			}

			reason = map[string]any{reasonCode: "ValidationError", "Message": awsErr.Message}
		}

		reasons[i], codes[i] = reason, reason[reasonCode].(string)
		canceled = canceled || codes[i] != "None"
	}

	if canceled {
		return &awserror.Error{
			Code: "TransactionCanceledException",
			Message: "Transaction cancelled, please refer cancellation reasons for specific reasons [" +
				strings.Join(codes, ", ") + "]",
			StatusCode: 400,
			Fields:     map[string]any{"CancellationReasons": reasons},
		}
	}

	for i, w := range writes {
		st := stages[i]
		if (w.delete && !st.found) || (!w.delete && st.next == nil) {
			continue
		}

		if err := writeItem(tx, w.req.key, st.t, st.record, st.old, st.next); err != nil {
			return err
		}
	}

	return nil
}
