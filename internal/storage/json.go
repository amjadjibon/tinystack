package storage

import (
	"encoding/json"
	"fmt"
)

// Read unmarshals the JSON record at k in tx into a new T.
// It returns (nil, false, nil) if k is not found.
func Read[T any](tx ReadTx, k string) (v *T, found bool, err error) {
	data, ok := tx.Get(k)
	if !ok {
		return nil, false, nil
	}

	v = new(T)
	if err = json.Unmarshal(data, v); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", k, err)
	}

	return v, true, nil
}

// Write marshals v as JSON and stages it under k in tx.
func Write(tx WriteTx, k string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tx.Put(k, data)
	return nil
}

// Records reads every JSON record under keyPrefix in tx, in key order.
func Records[T any](tx ReadTx, keyPrefix string) ([]*T, error) {
	var out []*T
	for _, k := range tx.Keys(keyPrefix) {
		v, _, err := Read[T](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}
