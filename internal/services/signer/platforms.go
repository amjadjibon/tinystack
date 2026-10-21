package signer

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	"github.com/amjadjibon/tinystack/internal/common"
)

// AWS's documented IoT platform metadata and managed Lambda/Notation platforms.
// https://docs.aws.amazon.com/cli/latest/userguide/cli_signer_code_examples.html
var platforms = []api.SigningPlatform{
	platform(
		"AWSIoTDeviceManagement-SHA256-ECDSA",
		"AWS IoT Device Management SHA256-ECDSA",
		"AWSIoTDeviceManagement",
		"SHA256-ECDSA",
		"ECDSA",
		"SHA256",
		"JSONDetached",
		2048,
		false,
	),
	platform(
		"AWSLambda-SHA384-ECDSA",
		"AWS Lambda",
		"AWSLambda",
		"SHA384-ECDSA",
		"ECDSA",
		"SHA384",
		"JSONDetached",
		250,
		true,
	),
	platform(
		"AmazonFreeRTOS-Default",
		"Amazon FreeRTOS SHA256-ECDSA",
		"AmazonFreeRTOS",
		"SHA256-ECDSA",
		"ECDSA",
		"SHA256",
		"JSONEmbedded",
		16,
		false,
	),
	platform(
		"AmazonFreeRTOS-TI-CC3220SF",
		"Amazon FreeRTOS SHA1-RSA CC3220SF-Format",
		"AmazonFreeRTOS",
		"SHA1-RSA-TISHA1",
		"RSA",
		"SHA1",
		"JSONEmbedded",
		16,
		false,
	),
	platform(
		"Notation-OCI-SHA384-ECDSA",
		"Notation for container registries",
		"Notation",
		"SHA384-ECDSA",
		"ECDSA",
		"SHA384",
		"JSONDetached",
		0,
		true,
	),
}

func platform(
	id, display, partner, target, encryption, hash, format string,
	size int64,
	revoke bool,
) api.SigningPlatform {
	allowed := []string{encryption}
	if id == "AmazonFreeRTOS-Default" {
		allowed = append(allowed, "RSA")
	}

	return api.SigningPlatform{
		PlatformId: new(id), DisplayName: new(display), Partner: new(partner), Target: new(target),
		Category: new("AWS"), MaxSizeInMB: new(size), RevocationSupported: new(revoke),
		SigningConfiguration: &api.SigningConfiguration{
			EncryptionAlgorithmOptions: &api.EncryptionAlgorithmOptions{
				DefaultValue:  new(encryption),
				AllowedValues: allowed,
			},
			HashAlgorithmOptions: &api.HashAlgorithmOptions{
				DefaultValue:  new(hash),
				AllowedValues: []string{hash},
			},
		},
		SigningImageFormat: &api.SigningImageFormat{DefaultFormat: new(format), SupportedFormats: []string{format}},
	}
}

func platformByID(id *string) (*api.SigningPlatform, error) {
	for _, p := range platforms {
		if *p.PlatformId == common.Value(id) {
			return common.Clone(&p), nil
		}
	}

	return nil, missing()
}

// GetSigningPlatform returns fixed signing configuration and image-format metadata.
func (s *Service) GetSigningPlatform(
	_ context.Context,
	in *api.GetSigningPlatformRequest,
) (*api.GetSigningPlatformResponse, error) {
	p, err := platformByID(in.PlatformId)
	if err != nil {
		return nil, err
	}

	return &api.GetSigningPlatformResponse{
		PlatformId:           p.PlatformId,
		DisplayName:          p.DisplayName,
		Partner:              p.Partner,
		Target:               p.Target,
		Category:             p.Category,
		SigningConfiguration: p.SigningConfiguration,
		SigningImageFormat:   p.SigningImageFormat,
		MaxSizeInMB:          p.MaxSizeInMB,
		RevocationSupported:  p.RevocationSupported,
	}, nil
}

// ListSigningPlatforms filters the fixed platform table and paginates by platform ID.
func (s *Service) ListSigningPlatforms(
	_ context.Context,
	in *api.ListSigningPlatformsRequest,
) (*api.ListSigningPlatformsResponse, error) {
	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	rows := []api.SigningPlatform{}
	for _, p := range platforms {
		if in.Category != nil && *in.Category != *p.Category {
			continue
		}

		if in.Partner != nil && *in.Partner != *p.Partner {
			continue
		}

		if in.Target != nil && *in.Target != *p.Target {
			continue
		}

		rows = append(rows, *common.Clone(&p))
	}

	page, next, err := common.Page(
		rows,
		func(p api.SigningPlatform) string { return *p.PlatformId },
		in.NextToken,
		n,
		invalid("Invalid NextToken."),
	)
	return &api.ListSigningPlatformsResponse{Platforms: page, NextToken: next}, err
}
