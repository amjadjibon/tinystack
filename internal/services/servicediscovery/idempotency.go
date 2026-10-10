package servicediscovery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Request tokens live under the same regional scope and replay the committed result.
type tokenRecord struct{ Input, Result string }

func tokenKey(scope request.Scope, action string, token *string) string {
	return scope.Key(serviceName, "token", action+fmt.Sprintf("-%x", sha256.Sum256([]byte(common.Value(token)))))
}

func replay(tx storage.ReadTx, scope request.Scope, action string, token *string, input any) (string, error) {
	if common.Value(token) == "" {
		return "", nil
	}

	previous, found, err := storage.Read[tokenRecord](tx, tokenKey(scope, action, token))
	if err != nil || !found {
		return "", err
	}

	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}

	if previous.Input != string(data) {
		return "", awserror.New("DuplicateRequest", "Token already used with different input")
	}

	return previous.Result, nil
}

func remember(tx storage.WriteTx, scope request.Scope, action string, token *string, input any, result string) error {
	if common.Value(token) == "" {
		return nil
	}

	data, err := json.Marshal(input)
	if err != nil {
		return err
	}

	return storage.Write(tx, tokenKey(scope, action, token), tokenRecord{Input: string(data), Result: result})
}
