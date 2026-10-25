package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iot"
	"github.com/aws/aws-sdk-go-v2/service/iot/types"
)

func TestIoTThingPrincipalsV2(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := iotClient(s, "test")
	name := new("typed-principals")
	if _, err := c.CreateThing(ctx, &iot.CreateThingInput{ThingName: name}); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []types.ThingPrincipalType{
		types.ThingPrincipalTypeExclusiveThing, types.ThingPrincipalTypeNonExclusiveThing,
	} {
		cert, err := c.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{})
		if err != nil {
			t.Fatal(err)
		}

		if _, err = c.AttachThingPrincipal(ctx, &iot.AttachThingPrincipalInput{
			ThingName: name, Principal: cert.CertificateArn, ThingPrincipalType: kind,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = iotClient(s, "test")
	first, err := c.ListThingPrincipalsV2(
		ctx,
		&iot.ListThingPrincipalsV2Input{ThingName: name, MaxResults: new(int32(1))},
	)
	if err != nil || len(first.ThingPrincipalObjects) != 1 || first.NextToken == nil {
		t.Fatalf("page 1: %+v %v", first, err)
	}

	second, err := c.ListThingPrincipalsV2(ctx, &iot.ListThingPrincipalsV2Input{
		ThingName: name, MaxResults: new(int32(1)), NextToken: first.NextToken,
	})
	if err != nil || len(second.ThingPrincipalObjects) != 1 || second.NextToken != nil ||
		*first.ThingPrincipalObjects[0].Principal == *second.ThingPrincipalObjects[0].Principal {
		t.Fatalf("page 2: %+v %v", second, err)
	}

	filtered, err := c.ListThingPrincipalsV2(ctx, &iot.ListThingPrincipalsV2Input{
		ThingName: name, ThingPrincipalType: types.ThingPrincipalTypeExclusiveThing,
	})
	if err != nil || len(filtered.ThingPrincipalObjects) != 1 ||
		filtered.ThingPrincipalObjects[0].ThingPrincipalType != types.ThingPrincipalTypeExclusiveThing {
		t.Fatalf("filter: %+v %v", filtered, err)
	}

	if _, err = c.ListThingPrincipalsV2(ctx, &iot.ListThingPrincipalsV2Input{ThingName: new("missing")}); err == nil {
		t.Fatal("missing thing accepted")
	}

	if _, err = c.ListThingPrincipalsV2(ctx, &iot.ListThingPrincipalsV2Input{
		ThingName: name, NextToken: new("!"),
	}); err == nil {
		t.Fatal("invalid token accepted")
	}
	if _, err = c.ListThingPrincipalsV2(ctx, &iot.ListThingPrincipalsV2Input{
		ThingName: name, ThingPrincipalType: types.ThingPrincipalType("unsupported"),
	}); err == nil {
		t.Fatal("unsupported attachment type accepted")
	}
}
