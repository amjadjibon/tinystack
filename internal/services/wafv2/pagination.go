package wafv2

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Bind cursors to the account, Region, API and filter/parent. The shared Page
// helper retains its resume-after-deletion behavior for the last resource ID.
func page[T any](
	rows []T,
	key func(T) string,
	token *string,
	maximum *int64,
	scope request.Scope,
	query string,
) (selected []T, next *string, err error) {
	limit := common.Value(common.OrElse(maximum, new(int64(100))))
	if limit < 1 || limit > 100 {
		return nil, nil, invalid("Limit must be between 1 and 100")
	}
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/" + query))
	prefix := hex.EncodeToString(sum[:]) + ":"
	bad := invalid("Invalid NextMarker for this account, Region, operation or filter")
	var inner *string
	if token != nil {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || !strings.HasPrefix(string(decoded), prefix) {
			return nil, nil, bad
		}
		value := strings.TrimPrefix(string(decoded), prefix)
		inner = new(base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	selected, next, err = common.Page(rows, key, inner, int(limit), bad)
	if next != nil {
		last, _ := base64.RawURLEncoding.DecodeString(*next)
		next = new(base64.RawURLEncoding.EncodeToString([]byte(prefix + string(last))))
	}
	return selected, next, err
}
