package globalaccelerator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func ipSets(slot int, dual bool) []api.IpSet {
	families := []string{"IPv4"}

	addresses := [][]string{{fmt.Sprintf("192.0.2.%d", slot), fmt.Sprintf("198.51.100.%d", slot)}}

	if dual {
		families = append(families, "IPv6")
		addresses = append(
			addresses,
			[]string{fmt.Sprintf("2001:db8::%x", slot*2+1), fmt.Sprintf("2001:db8::%x", slot*2+2)},
		)
	}

	out := make([]api.IpSet, 0, len(families))
	for i, family := range families {
		details := []api.IpAddressDetail{}

		for j, ip := range addresses[i] {
			details = append(
				details,
				api.IpAddressDetail{IpAddress: new(ip), NetworkZone: new(fmt.Sprintf("zone-%d", j+1))},
			)
		}

		out = append(
			out,
			api.IpSet{
				IpFamily:         new(family),
				IpAddressFamily:  new(family),
				IpAddresses:      addresses[i],
				IpAddressDetails: details,
			},
		)
	}

	return out
}

func configureIPs(r *record) {
	a := r.Accelerator
	dual := common.Value(a.IpAddressType) == "DUAL_STACK"
	a.IpSets = ipSets(r.Slot, dual)
	a.DualStackDnsName = nil
	if dual {
		a.DualStackDnsName = new(
			strings.Replace(
				common.Value(a.DnsName),
				".awsglobalaccelerator.com",
				".dualstack.awsglobalaccelerator.com",
				1,
			),
		)
	}
}

func pending(a *api.Accelerator) *api.Accelerator {
	out := common.Clone(a)
	out.Status = new("IN_PROGRESS")
	return out
}

// CreateAccelerator creates a standard accelerator with static documentation addresses.
func (s *Service) CreateAccelerator(
	ctx context.Context,
	in *api.CreateAcceleratorRequest,
) (*api.CreateAcceleratorResponse, error) {
	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{
		Kind: kindAccelerator,

		Tags:       tags,
		Attributes: &api.AcceleratorAttributes{FlowLogsEnabled: new(false)},
		Accelerator: &api.Accelerator{
			Name:          in.Name,
			IpAddressType: in.IpAddressType,
			Enabled:       in.Enabled,
			Status:        new("DEPLOYED"),
		},
	}

	if r.Accelerator.IpAddressType == nil {
		r.Accelerator.IpAddressType = new("IPV4")
	}

	if r.Accelerator.Enabled == nil {
		r.Accelerator.Enabled = new(true)
	}

	r, err = s.insert(
		ctx,
		r,
		in.IdempotencyToken,
		in,
		func(_ storage.WriteTx, scope request.Scope, r *record, all []*record) error {
			used := map[int]bool{}

			for _, old := range all {
				used[old.Slot] = true
			}

			r.Slot = 1
			for r.Slot < 255 && used[r.Slot] {
				r.Slot++
			}

			if r.Slot == 255 {
				return awserror.New("LimitExceededException", "TinyStack documentation IPv4 address pool is exhausted")
			}

			id := common.RandomID(32, "0123456789abcdef")
			id = fmt.Sprintf("%s-%s-%s-%s-%s", id[:8], id[8:12], id[12:16], id[16:20], id[20:])
			r.ARN = "arn:aws:globalaccelerator::" + scope.AccountID + ":accelerator/" + id
			a := r.Accelerator
			a.AcceleratorArn = new(r.ARN)
			hash := sha256.Sum256([]byte(r.ARN))
			a.DnsName = new(fmt.Sprintf("a%x.awsglobalaccelerator.com", hash[:8]))
			a.CreatedTime = common.SecondsMilli(time.Now())
			a.LastModifiedTime = a.CreatedTime
			configureIPs(r)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateAcceleratorResponse{Accelerator: pending(r.Accelerator)}, nil
}

// DescribeAccelerator returns the deployed accelerator record.
func (s *Service) DescribeAccelerator(
	ctx context.Context,
	in *api.DescribeAcceleratorRequest,
) (*api.DescribeAcceleratorResponse, error) {
	r, err := s.get(ctx, in.AcceleratorArn, kindAccelerator)
	if err != nil {
		return nil, err
	}

	return &api.DescribeAcceleratorResponse{Accelerator: r.Accelerator}, nil
}

// ListAccelerators pages account-global accelerators in storage key order.
func (s *Service) ListAccelerators(
	ctx context.Context,
	in *api.ListAcceleratorsRequest,
) (*api.ListAcceleratorsResponse, error) {
	records, next, err := s.list(ctx, kindAccelerator, "", in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListAcceleratorsResponse{Accelerators: []api.Accelerator{}, NextToken: next}

	for _, r := range records {
		out.Accelerators = append(out.Accelerators, *r.Accelerator)
	}

	return out, nil
}

// UpdateAccelerator updates accelerator settings and returns an IN_PROGRESS snapshot.
func (s *Service) UpdateAccelerator(
	ctx context.Context,
	in *api.UpdateAcceleratorRequest,
) (*api.UpdateAcceleratorResponse, error) {
	r, err := s.change(
		ctx,
		in.AcceleratorArn,
		kindAccelerator,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			a := r.Accelerator
			if in.Name != nil {
				a.Name = in.Name
			}

			if in.Enabled != nil {
				a.Enabled = in.Enabled
			}

			if in.IpAddressType != nil {
				a.IpAddressType = in.IpAddressType
			}

			configureIPs(r)
			a.LastModifiedTime = common.SecondsMilli(time.Now())
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAcceleratorResponse{Accelerator: pending(r.Accelerator)}, nil
}

// DeleteAccelerator removes a disabled accelerator after all listeners are deleted.
func (s *Service) DeleteAccelerator(
	ctx context.Context,
	in *api.DeleteAcceleratorRequest,
) (*api.DeleteAcceleratorOutput, error) {
	if err := s.remove(ctx, in.AcceleratorArn, kindAccelerator); err != nil {
		return nil, err
	}

	return &api.DeleteAcceleratorOutput{}, nil
}

// DescribeAcceleratorAttributes returns the stored flow log configuration.
func (s *Service) DescribeAcceleratorAttributes(
	ctx context.Context,
	in *api.DescribeAcceleratorAttributesRequest,
) (*api.DescribeAcceleratorAttributesResponse, error) {
	r, err := s.get(ctx, in.AcceleratorArn, kindAccelerator)
	if err != nil {
		return nil, err
	}

	return &api.DescribeAcceleratorAttributesResponse{AcceleratorAttributes: r.Attributes}, nil
}

// UpdateAcceleratorAttributes stores flow log settings after checking the local bucket.
func (s *Service) UpdateAcceleratorAttributes(
	ctx context.Context,
	in *api.UpdateAcceleratorAttributesRequest,
) (*api.UpdateAcceleratorAttributesResponse, error) {
	// Resolve S3 outside our transaction; bucket ownership/existence uses S3's exported API.
	current, err := s.get(ctx, in.AcceleratorArn, kindAccelerator)
	if err != nil {
		return nil, err
	}

	bucket := current.Attributes.FlowLogsS3Bucket
	if in.FlowLogsS3Bucket != nil {
		bucket = in.FlowLogsS3Bucket
	}

	if common.Value(bucket) != "" {
		if s.buckets == nil {
			return nil, invalid("S3 service is unavailable")
		}

		// ListBuckets is account-wide, unlike HeadBucket, which requires the bucket's Region.
		m, e := request.Global(ctx)
		if e != nil {
			return nil, e
		}

		m.Region = "us-west-2"
		bucketCtx := request.WithMetadata(ctx, m)
		listed, e := s.buckets.ListBuckets(
			bucketCtx,
			&s3api.ListBucketsRequest{Prefix: bucket, MaxBuckets: new(int64(1))},
		)
		if e != nil {
			return nil, invalid("Cannot resolve flow logs bucket: %s", e)
		}

		if len(listed.Buckets) == 0 || common.Value(listed.Buckets[0].Name) != *bucket {
			return nil, invalid("Flow logs S3 bucket must exist in this account")
		}

		m.Region = common.Value(listed.Buckets[0].BucketRegion)
		if _, e = s.buckets.HeadBucket(
			request.WithMetadata(ctx, m),
			&s3api.HeadBucketRequest{Bucket: bucket},
		); e != nil {
			return nil, invalid("Flow logs S3 bucket must exist: %s", e)
		}
	}

	r, err := s.change(
		ctx,
		in.AcceleratorArn,
		kindAccelerator,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			a := r.Attributes
			if in.FlowLogsEnabled != nil {
				a.FlowLogsEnabled = in.FlowLogsEnabled
			}

			if in.FlowLogsS3Bucket != nil {
				a.FlowLogsS3Bucket = in.FlowLogsS3Bucket
			}

			if in.FlowLogsS3Prefix != nil {
				a.FlowLogsS3Prefix = in.FlowLogsS3Prefix
			}

			if common.Value(a.FlowLogsEnabled) &&
				(common.Value(a.FlowLogsS3Bucket) == "" || a.FlowLogsS3Prefix == nil) {
				return invalid("FlowLogsS3Bucket and FlowLogsS3Prefix are required when flow logs are enabled")
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAcceleratorAttributesResponse{AcceleratorAttributes: r.Attributes}, nil
}
