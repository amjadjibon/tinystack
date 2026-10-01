package sqs

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultReusePeriod = 300
	// managedKeyID is SQS's AWS managed key, which needs no check.
	managedKeyID = "alias/aws/sqs"
)

// optionalAttributes are the known attributes a queue may not have, which
// GetQueueAttributes omits instead of rejecting.
var optionalAttributes = map[string]bool{
	attrRedrivePolicy:             true,
	attrRedriveAllowPolicy:        true,
	attrFifoQueue:                 true,
	attrContentBasedDeduplication: true,
	attrDeduplicationScope:        true,
	attrFifoThroughputLimit:       true,
	attrKmsMasterKeyID:            true,
	attrKmsDataKeyReusePeriod:     true,
}

// kmsCodes maps KMS's errors to the codes SQS reports them with.
var kmsCodes = map[string]string{
	"DisabledException":        "KmsDisabled",
	"KMSInvalidStateException": "KmsInvalidState",
	"NotFoundException":        "KmsNotFound",
}

func (st *settings) reusePeriod() int64 {
	if st.KmsDataKeyReusePeriodSeconds == 0 {
		return defaultReusePeriod
	}

	return st.KmsDataKeyReusePeriodSeconds
}

// useKey reports why the KMS key of the queue stored under key cannot encrypt
// a message. Messages are stored as sent; the check is what SSE-KMS adds.
func (s *Service) useKey(ctx context.Context, key string) error {
	var id string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		id = q.Settings.KmsMasterKeyID
		return nil
	})
	if err != nil || id == "" || id == managedKeyID {
		return err
	}

	// The key belongs to the queue's account and region, not the sender's.
	parts := strings.SplitN(key, "/", 3)
	err = s.keys.Check(ctx, request.Scope{AccountID: parts[0], Region: parts[1]}, id)
	e, ok := errors.AsType[*awserror.Error](err)
	if !ok {
		return err
	}

	code := kmsCodes[e.Code]
	if code == "" {
		code = "KmsNotFound"
	}

	return awserror.New(code, e.Message)
}
