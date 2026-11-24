package s3

import (
	"crypto/md5" // #nosec G501 -- S3 uses an MD5 digest to validate the customer key.
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/common"
)

func customerKeyMD5(algorithm, key, checksum *string) (string, error) {
	if algorithm == nil && key == nil && checksum == nil {
		return "", nil
	}

	if common.Value(algorithm) != sseS3 || key == nil || checksum == nil {
		return "", fail(http.StatusBadRequest, "InvalidRequest", "SSE-C requires AES256, a customer key and its MD5")
	}

	raw, err := base64.StdEncoding.Strict().DecodeString(*key)
	if err != nil || len(raw) != 32 {
		return "", invalidArgument("An SSE-C key must be a 256-bit base64-encoded key")
	}

	sum := md5.Sum(raw) // #nosec G401 -- S3 validates keys against their protocol MD5 digest.
	encoded := base64.StdEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(encoded), []byte(*checksum)) != 1 {
		return "", fail(http.StatusBadRequest, "InvalidDigest", "The SSE-C key did not match its MD5")
	}

	return encoded, nil
}

func requireCustomerKey(stored, supplied string) error {
	switch {
	case stored == "" && supplied == "":
		return nil
	case stored == "":
		return fail(http.StatusBadRequest, "InvalidRequest", "The object does not use SSE-C")
	case supplied == "":
		return fail(http.StatusBadRequest, "InvalidRequest", "The object requires an SSE-C key")
	case subtle.ConstantTimeCompare([]byte(stored), []byte(supplied)) != 1:
		return accessDenied()
	default:
		return nil
	}
}
