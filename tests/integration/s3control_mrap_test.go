package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	"github.com/aws/aws-sdk-go-v2/service/s3control/types"
)

func awaitMRAP(t *testing.T, c *s3control.Client, token *string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		out, err := c.DescribeMultiRegionAccessPointOperation(
			ctx,
			&s3control.DescribeMultiRegionAccessPointOperationInput{
				AccountId:       new("000000000000"),
				RequestTokenARN: token,
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		if *out.AsyncOperation.RequestStatus == "SUCCEEDED" {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestS3ControlMRAP(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := s3ControlClient(srv)
	b := s3Client(srv, "test", "us-east-1")
	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("mrap-bucket")}); err != nil {
		t.Fatal(err)
	}

	account := new("000000000000")
	input := &s3control.CreateMultiRegionAccessPointInput{
		AccountId:   account,
		ClientToken: new("create-mrap-token"),
		Details: &types.CreateMultiRegionAccessPointInput{
			Name:    new("test-mrap"),
			Regions: []types.Region{{Bucket: new("mrap-bucket")}},
		},
	}

	out, err := c.CreateMultiRegionAccessPoint(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.CreateMultiRegionAccessPoint(ctx, input)
	if err != nil || *again.RequestTokenARN != *out.RequestTokenARN {
		t.Fatalf("idempotency %+v %v", again, err)
	}

	changed := *input
	details := *input.Details
	details.Name = new("changed")
	changed.Details = &details
	_, err = c.CreateMultiRegionAccessPoint(ctx, &changed)
	wantAccessAnalyzerError(t, err, "InvalidRequest")
	pending, err := c.DescribeMultiRegionAccessPointOperation(
		ctx,
		&s3control.DescribeMultiRegionAccessPointOperationInput{
			AccountId:       account,
			RequestTokenARN: out.RequestTokenARN,
		},
	)
	if err != nil ||
		(*pending.AsyncOperation.RequestStatus != "INPROGRESS" && *pending.AsyncOperation.RequestStatus != "SUCCEEDED") {
		t.Fatalf("pending %+v %v", pending, err)
	}

	stop()
	srv, stop = persistent(t, dir, &fakeRuntime{})
	c = s3ControlClient(srv)
	awaitMRAP(t, c, out.RequestTokenARN)
	got, err := c.GetMultiRegionAccessPoint(
		ctx,
		&s3control.GetMultiRegionAccessPointInput{AccountId: account, Name: new("test-mrap")},
	)
	if err != nil || got.AccessPoint.Status != types.MultiRegionAccessPointStatusReady {
		t.Fatalf("get %+v %v", got, err)
	}

	list, err := c.ListMultiRegionAccessPoints(ctx, &s3control.ListMultiRegionAccessPointsInput{AccountId: account})
	if err != nil || len(list.AccessPoints) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}

	second, err := c.CreateMultiRegionAccessPoint(
		ctx,
		&s3control.CreateMultiRegionAccessPointInput{
			AccountId: account,
			Details: &types.CreateMultiRegionAccessPointInput{
				Name:    new("another-mrap"),
				Regions: []types.Region{{Bucket: new("mrap-bucket")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	awaitMRAP(t, c, second.RequestTokenARN)
	page, err := c.ListMultiRegionAccessPoints(
		ctx,
		&s3control.ListMultiRegionAccessPointsInput{AccountId: account, MaxResults: 1},
	)
	if err != nil || len(page.AccessPoints) != 1 || page.NextToken == nil {
		t.Fatalf("page %+v %v", page, err)
	}

	page, err = c.ListMultiRegionAccessPoints(
		ctx,
		&s3control.ListMultiRegionAccessPointsInput{AccountId: account, MaxResults: 1, NextToken: page.NextToken},
	)
	if err != nil || len(page.AccessPoints) != 1 || page.NextToken != nil {
		t.Fatalf("page2 %+v %v", page, err)
	}

	deleted, err := c.DeleteMultiRegionAccessPoint(
		ctx,
		&s3control.DeleteMultiRegionAccessPointInput{
			AccountId: account,
			Details:   &types.DeleteMultiRegionAccessPointInput{Name: new("another-mrap")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	awaitMRAP(t, c, deleted.RequestTokenARN)

	arn := new("arn:aws:s3::000000000000:accesspoint/" + *got.AccessPoint.Alias)
	routes, err := c.GetMultiRegionAccessPointRoutes(
		ctx,
		&s3control.GetMultiRegionAccessPointRoutesInput{AccountId: account, Mrap: arn},
	)
	if err != nil || len(routes.Routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}

	if _, err = c.SubmitMultiRegionAccessPointRoutes(
		ctx,
		&s3control.SubmitMultiRegionAccessPointRoutesInput{
			AccountId: account,
			Mrap:      arn,
			RouteUpdates: []types.MultiRegionAccessPointRoute{
				{Bucket: new("mrap-bucket"), TrafficDialPercentage: new(int32(100))},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	policy := new(
		`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"}` +
			`,"Action":"s3:GetObject","Resource":"` + *arn + `/object/*"}]}`,
	)
	pol, err := c.PutMultiRegionAccessPointPolicy(
		ctx,
		&s3control.PutMultiRegionAccessPointPolicyInput{
			AccountId:   account,
			ClientToken: new("policy-mrap-token"),
			Details:     &types.PutMultiRegionAccessPointPolicyInput{Name: new("test-mrap"), Policy: policy},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	awaitMRAP(t, c, pol.RequestTokenARN)
	p, err := c.GetMultiRegionAccessPointPolicy(
		ctx,
		&s3control.GetMultiRegionAccessPointPolicyInput{AccountId: account, Name: new("test-mrap")},
	)
	if err != nil || *p.Policy.Established.Policy != *policy {
		t.Fatalf("policy %+v %v", p, err)
	}

	status, err := c.GetMultiRegionAccessPointPolicyStatus(
		ctx,
		&s3control.GetMultiRegionAccessPointPolicyStatusInput{AccountId: account, Name: new("test-mrap")},
	)
	if err != nil || status.Established.IsPublic {
		t.Fatalf("status %+v %v", status, err)
	}

	del, err := c.DeleteMultiRegionAccessPoint(
		ctx,
		&s3control.DeleteMultiRegionAccessPointInput{
			AccountId:   account,
			ClientToken: new("delete-mrap-token"),
			Details:     &types.DeleteMultiRegionAccessPointInput{Name: new("test-mrap")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	awaitMRAP(t, c, del.RequestTokenARN)
	if _, err = c.GetMultiRegionAccessPoint(
		ctx,
		&s3control.GetMultiRegionAccessPointInput{AccountId: account, Name: new("test-mrap")},
	); err == nil {
		t.Fatal("deleted MRAP exists")
	}
}
