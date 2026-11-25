package sns

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	attrKmsMasterKeyID = "KmsMasterKeyId"
	// managedKeyID is SNS's AWS managed key, which needs no check.
	managedKeyID = "alias/aws/sns"
	maxKeyIDLen  = 2048
)

// kmsCodes maps KMS's errors to the codes SNS reports them with.
var kmsCodes = map[string]string{
	"DisabledException":        "KMSDisabledException",
	"KMSInvalidStateException": "KMSInvalidStateException",
	"NotFoundException":        "KMSNotFoundException",
	"AccessDeniedException":    "KMSAccessDeniedException",
}

func checkKmsMasterKeyID(v string) error {
	if len(v) > maxKeyIDLen {
		return invalid("KmsMasterKeyId must be at most %d characters", maxKeyIDLen)
	}

	return nil
}

// useKey reports why the KMS key of the topic stored under key cannot encrypt
// a message. Messages are stored as sent; the check is what SSE-KMS adds.
func (s *Service) useKey(ctx context.Context, key string) error {
	if s.keys == nil {
		return nil
	}

	var id string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		id = t.Attributes[attrKmsMasterKeyID]
		return nil
	})
	if err != nil || id == "" || id == managedKeyID {
		return err
	}

	// The key belongs to the topic's account and region, not the publisher's.
	parts := strings.SplitN(key, "/", 3)
	err = s.keys.Check(ctx, request.Scope{AccountID: parts[0], Region: parts[1]}, id)
	e, ok := errors.AsType[*awserror.Error](err)
	if !ok {
		return err
	}

	code := kmsCodes[e.Code]
	if code == "" {
		code = "KMSNotFoundException"
	}

	return awserror.New(code, e.Message)
}
