package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReadTx exposes a consistent snapshot; handles must stay inside the callback.
type ReadTx interface {
	Get(string) ([]byte, bool)
	// Keys returns, in order, the keys that start with prefix.
	Keys(prefix string) []string
}

// WriteTx stages changes which are committed together on callback success.
type WriteTx interface {
	ReadTx
	Put(string, []byte)
	Delete(string)
}

// Store runs serializable transactions; callbacks must not perform external I/O.
type Store interface {
	View(context.Context, func(ReadTx) error) error
	Update(context.Context, func(WriteTx) error) error
}

// Record is an opaque key-value pair.
type Record struct {
	Key   string
	Value []byte
}

type recordWire struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// MarshalJSON encodes Record with key and value as lossless base64 byte slices.
func (r Record) MarshalJSON() ([]byte, error) {
	val := r.Value
	if val == nil {
		val = []byte{}
	}

	return json.Marshal(recordWire{
		Key:   []byte(r.Key),
		Value: val,
	})
}

// UnmarshalJSON decodes Record with key and value from base64 strings.
// It strictly validates the v1 wire format: key and value must both be present
// as string-encoded base64 values, without duplicates, nulls, or numeric arrays.
func (r *Record) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("record must be a JSON object")
	}

	var (
		hasKey   bool
		hasValue bool
		keyStr   string
		valStr   string
	)

	for dec.More() {
		propTok, pErr := dec.Token()
		if pErr != nil {
			return pErr
		}
		prop, isStr := propTok.(string)
		if !isStr {
			return errors.New("record object property key must be a string")
		}

		valTok, vErr := dec.Token()
		if vErr != nil {
			return vErr
		}
		strVal, isValStr := valTok.(string)
		if !isValStr {
			return fmt.Errorf("record field %q must be a base64 string", prop)
		}

		switch prop {
		case "key":
			if hasKey {
				return errors.New("duplicate key field in record")
			}
			hasKey = true
			keyStr = strVal
		case "value":
			if hasValue {
				return errors.New("duplicate value field in record")
			}
			hasValue = true
			valStr = strVal
		default:
			return fmt.Errorf("unexpected field in record: %q", prop)
		}
	}

	tok, err = dec.Token()
	if err != nil {
		return err
	}
	delim, ok = tok.(json.Delim)
	if !ok || delim != '}' {
		return errors.New("malformed record JSON object")
	}

	if dec.More() {
		return errors.New("unexpected trailing tokens in record JSON")
	}
	if _, eofErr := dec.Token(); eofErr != io.EOF {
		return errors.New("unexpected trailing content in record JSON")
	}

	if !hasKey {
		return errors.New("missing required key field in record")
	}
	if !hasValue {
		return errors.New("missing required value field in record")
	}

	keyBytes, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return fmt.Errorf("decode record key base64: %w", err)
	}

	valBytes, err := base64.StdEncoding.DecodeString(valStr)
	if err != nil {
		return fmt.Errorf("decode record value base64: %w", err)
	}

	r.Key = string(keyBytes)
	r.Value = valBytes
	if r.Value == nil {
		r.Value = []byte{}
	}

	return nil
}

// Clear deletes every record in one transaction, so concurrent callers see the
// state before or after, never part of it. Services that poll the store find it
// empty and carry on.
func Clear(ctx context.Context, store Store) error {
	return store.Update(ctx, func(tx WriteTx) error {
		for _, key := range tx.Keys("") {
			tx.Delete(key)
		}

		return nil
	})
}

// Scoped splits a key laid out as "<account>/<region>/<service>/<rest>" and
// reports whether it belongs to service.
func Scoped(key, service string) (account, region, rest string, ok bool) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) != 4 || parts[2] != service {
		return "", "", "", false
	}

	return parts[0], parts[1], parts[3], true
}
