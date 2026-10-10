package config

import (
	"context"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var topicName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (s *Service) PutDeliveryChannel(
	ctx context.Context,
	in *api.PutDeliveryChannelRequest,
) (*api.PutDeliveryChannelOutput, error) {
	if in.DeliveryChannel == nil {
		return nil, invalid("DeliveryChannel is required")
	}
	channel := common.Clone(in.DeliveryChannel)
	name := common.Value(common.OrElse(channel.Name, new(defaultName)))
	if !validName(name, 256) {
		return nil, failure("InvalidDeliveryChannelNameException", "Invalid delivery channel name")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	if !bucketName.MatchString(common.Value(channel.S3BucketName)) {
		return nil, invalid("A valid s3BucketName is required")
	}
	if channel.S3KeyPrefix != nil &&
		(!length(channel.S3KeyPrefix, 0, 1024) || strings.ContainsAny(*channel.S3KeyPrefix, "\r\n\x00")) {
		return nil, failure("InvalidS3KeyPrefixException", "Invalid S3 key prefix")
	}
	if channel.SnsTopicARN != nil {
		prefix := "arn:aws:sns:" + scope.Region + ":" + scope.AccountID + ":"
		if !strings.HasPrefix(*channel.SnsTopicARN, prefix) ||
			!topicName.MatchString(strings.TrimPrefix(*channel.SnsTopicARN, prefix)) {
			return nil, failure(
				"InvalidSNSTopicARNException",
				"Only standard SNS topic ARNs in the caller's account and Region are supported",
			)
		}
	}
	if prop := channel.ConfigSnapshotDeliveryProperties; prop != nil &&
		!slices.Contains(frequencies, common.Value(prop.DeliveryFrequency)) {
		return nil, invalid("configSnapshotDeliveryProperties.deliveryFrequency is required and must be valid")
	}
	// Cross-service I/O must remain outside the Config transaction. This checks
	// existence only; bucket policies, IAM permissions, and delivery are not run.
	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: channel.S3BucketName}); err != nil {
		return nil, failure("NoSuchBucketException", "The destination S3 bucket does not exist")
	}
	channel.Name = new(name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if len(tx.Keys(scope.Prefix(serviceName, kindRecorder))) == 0 {
			return failure(
				"NoAvailableConfigurationRecorderException",
				"Create a configuration recorder before a delivery channel",
			)
		}
		keys := tx.Keys(scope.Prefix(serviceName, kindChannel))
		if len(keys) > 0 && keys[0] != scope.Key(serviceName, kindChannel, name) {
			return failure(
				"MaxNumberOfDeliveryChannelsExceededException",
				"Only one delivery channel per account and Region",
			)
		}
		return save(tx, scope, &record{Name: name, Kind: kindChannel, Channel: channel})
	})
	return &api.PutDeliveryChannelOutput{}, err
}

func (s *Service) DescribeDeliveryChannels(
	ctx context.Context,
	in *api.DescribeDeliveryChannelsRequest,
) (*api.DescribeDeliveryChannelsResponse, error) {
	records, err := s.list(ctx, kindChannel, in.DeliveryChannelNames)
	out := &api.DescribeDeliveryChannelsResponse{DeliveryChannels: []api.DeliveryChannel{}}
	for _, r := range records {
		out.DeliveryChannels = append(out.DeliveryChannels, *r.Channel)
	}
	return out, err
}

func (s *Service) DescribeDeliveryChannelStatus(
	ctx context.Context,
	in *api.DescribeDeliveryChannelStatusRequest,
) (*api.DescribeDeliveryChannelStatusResponse, error) {
	records, err := s.list(ctx, kindChannel, in.DeliveryChannelNames)
	out := &api.DescribeDeliveryChannelStatusResponse{DeliveryChannelsStatus: []api.DeliveryChannelStatus{}}
	// No snapshots, streams, or history have been delivered. Omit those statuses.
	for _, r := range records {
		out.DeliveryChannelsStatus = append(out.DeliveryChannelsStatus, api.DeliveryChannelStatus{Name: new(r.Name)})
	}
	return out, err
}

func (s *Service) DeleteDeliveryChannel(
	ctx context.Context,
	in *api.DeleteDeliveryChannelRequest,
) (*api.DeleteDeliveryChannelOutput, error) {
	return &api.DeleteDeliveryChannelOutput{}, s.remove(ctx, kindChannel, common.Value(in.DeliveryChannelName))
}
