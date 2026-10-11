package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// Batch tags are maps, so duplicate wire keys would otherwise be overwritten
// before validation. Walk only successfully decoded Batch documents.
func batchUniqueJSON(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		keys := map[string]bool{}
		for decoder.More() {
			if delim == '{' {
				keyToken, keyErr := decoder.Token()
				if keyErr != nil {
					return keyErr
				}
				key, _ := keyToken.(string)
				if keys[key] {
					return awserror.New("ClientException", "Duplicate JSON member or tag key: "+key)
				}
				keys[key] = true
			}
			if childErr := value(); childErr != nil {
				return childErr
			}
		}
		_, err = decoder.Token()
		return err
	}
	return value()
}
