package sfn

import (
	"context"
	"errors"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	encryptionAWSOwned        = "AWS_OWNED_KEY"
	encryptionCustomerManaged = "CUSTOMER_MANAGED_KMS_KEY"
	defaultKMSReusePeriod     = int64(300)
	includedAllData           = "ALL_DATA"
	includedMetadataOnly      = "METADATA_ONLY"
	errKMSInvalidState        = "KmsInvalidStateException"
	errKMSAccessDenied        = "KmsAccessDeniedException"
)

func defaultEncryption() *api.EncryptionConfiguration {
	return &api.EncryptionConfiguration{Type: new(encryptionAWSOwned)}
}

func encryptionOf(enc *api.EncryptionConfiguration) *api.EncryptionConfiguration {
	if enc == nil {
		return defaultEncryption()
	}

	cp := *enc
	return &cp
}

func normalizeEncryption(enc *api.EncryptionConfiguration) (*api.EncryptionConfiguration, error) {
	if enc == nil {
		return defaultEncryption(), nil
	}

	switch common.Value(enc.Type) {
	case encryptionAWSOwned:
		if enc.KmsKeyId != nil || enc.KmsDataKeyReusePeriodSeconds != nil {
			return nil, awserror.New(
				"InvalidEncryptionConfiguration",
				"kmsKeyId and kmsDataKeyReusePeriodSeconds cannot be set when type is AWS_OWNED_KEY",
			)
		}

		return defaultEncryption(), nil
	case encryptionCustomerManaged:
		keyID := common.Value(enc.KmsKeyId)
		if keyID == "" {
			return nil, awserror.New(
				"InvalidEncryptionConfiguration",
				"kmsKeyId is required when type is CUSTOMER_MANAGED_KMS_KEY",
			)
		}

		reuse := defaultKMSReusePeriod
		if enc.KmsDataKeyReusePeriodSeconds != nil {
			reuse = *enc.KmsDataKeyReusePeriodSeconds
			if reuse < 60 || reuse > 900 {
				return nil, awserror.New(
					"InvalidEncryptionConfiguration",
					"kmsDataKeyReusePeriodSeconds must be between 60 and 900",
				)
			}
		}

		return &api.EncryptionConfiguration{
			Type:                         new(encryptionCustomerManaged),
			KmsKeyId:                     new(keyID),
			KmsDataKeyReusePeriodSeconds: new(reuse),
		}, nil
	default:
		return nil, awserror.New(
			"InvalidEncryptionConfiguration",
			"type must be AWS_OWNED_KEY or CUSTOMER_MANAGED_KMS_KEY",
		)
	}
}

func encryptionEqual(a, b *api.EncryptionConfiguration) bool {
	ea, eb := encryptionOf(a), encryptionOf(b)
	return common.Value(ea.Type) == common.Value(eb.Type) &&
		common.Value(ea.KmsKeyId) == common.Value(eb.KmsKeyId) &&
		common.Value(ea.KmsDataKeyReusePeriodSeconds) == common.Value(eb.KmsDataKeyReusePeriodSeconds)
}

var kmsCodes = map[string]string{
	"DisabledException":             errKMSInvalidState,
	"KMSInvalidStateException":      errKMSInvalidState,
	"InvalidKeyUsageException":      errKMSInvalidState,
	"UnsupportedOperationException": errKMSInvalidState,
	"NotFoundException":             errKMSAccessDenied,
	"AccessDeniedException":         errKMSAccessDenied,
}

func (s *Service) checkEncryptionKey(
	ctx context.Context,
	scope request.Scope,
	enc *api.EncryptionConfiguration,
) error {
	if s.keys == nil || enc == nil || common.Value(enc.Type) != encryptionCustomerManaged {
		return nil
	}

	err := s.keys.Check(ctx, scope, common.Value(enc.KmsKeyId))
	if err == nil {
		return nil
	}

	ae, ok := errors.AsType[*awserror.Error](err)
	if !ok {
		return err
	}

	code := kmsCodes[ae.Code]
	if code == "" {
		code = errKMSAccessDenied
	}

	return awserror.New(code, ae.Message)
}

func metadataOnly(included *string) (bool, error) {
	if included == nil || *included == includedAllData {
		return false, nil
	}

	if *included == includedMetadataOnly {
		return true, nil
	}

	return false, awserror.Validation("includedData must be ALL_DATA or METADATA_ONLY")
}
