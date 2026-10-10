package cloudformation

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/wafv2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestWAFRetagCompensationAndConfiguration(t *testing.T) {
	for _, failure := range []string{
		"",
		"ListTagsForResource",
		opUntagResource,
		opTagResource,
		"limit",
		"canceled",
		"restore",
	} {
		t.Run(failure, func(t *testing.T) {
			scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
			ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
			s := wafv2.New(&memory.Store{})
			ip, err := s.CreateIPSet(
				ctx,
				&api.CreateIPSetRequest{
					Name:             new("primary"),
					Scope:            new("REGIONAL"),
					IPAddressVersion: new("IPV4"),
					Description:      new("Original configuration"),
					Addresses:        []string{"192.0.2.1/32"},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			old := map[string]string{}
			for i := range 48 {
				old[fmt.Sprintf("key%02d", i)] = "original"
			}
			live := maps.Clone(old)
			live["key00"] = "outside-template"
			live["external1"] = "keep1"
			live["external2"] = "keep2"
			set := []api.Tag{}
			for k, v := range live {
				set = append(set, api.Tag{Key: new(k), Value: new(v)})
			}
			_, err = s.TagResource(ctx, &api.TagResourceRequest{ResourceARN: ip.Summary.ARN, Tags: set})
			if err != nil {
				t.Fatal(err)
			}
			next := maps.Clone(old)
			delete(next, "key00")
			next["replacement"] = "new"
			next["key01"] = "updated"
			if failure == "limit" {
				next["one-too-many"] = "new"
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			descriptor := api.Descriptor(s)
			injected := awserror.New("WAFInternalErrorException", "injected failure")
			tagCalls := 0
			for _, operation := range []string{"ListTagsForResource", opUntagResource, opTagResource} {
				op := descriptor.Operations[operation]
				invoke := op.Invoke
				op.Invoke = func(ctx context.Context, input any) (any, error) {
					if operation == opTagResource {
						tagCalls++
						if failure == "restore" ||
							tagCalls == 1 && (failure == "canceled" || failure == opTagResource) {
							if failure == "canceled" {
								cancel()
							}
							return nil, injected
						}
					} else if failure == operation {
						return nil, injected
					}
					return invoke(ctx, input)
				}
				descriptor.Operations[operation] = op
			}
			c := &client{caller: catalog.NewCaller(descriptor), scope: scope}
			attributes := map[string]any{memberArn: *ip.Summary.ARN, wafID: *ip.Summary.Id}
			r := &resource{
				Physical:   "primary|" + *ip.Summary.Id + "|REGIONAL",
				Properties: props{memberTags: tagList(old, memberKey, memberValue)},
				Attributes: attributes,
			}
			p := props{
				memberTags:        tagList(next, memberKey, memberValue),
				wafScope:          "REGIONAL",
				wafFamily:         "IPV4",
				wafAddresses:      []any{"198.51.100.1/32"},
				memberDescription: "Changed configuration",
			}
			_, err = types["AWS::WAFv2::IPSet"].update(callCtx, c, r, p)
			want := maps.Clone(live)
			switch failure {
			case "":
				if err != nil {
					t.Fatal(err)
				}
				delete(want, "key00")
				want["replacement"] = "new"
				want["key01"] = "updated"
			case "limit":
				if awserror.From(err).Code != "WAFLimitsExceededException" {
					t.Fatalf("limit %v", err)
				}
			case "restore":
				if err == nil || !strings.Contains(err.Error(), "restoring WAF removed tags failed") {
					t.Fatalf("restore detail %v", err)
				}
				delete(want, "key00")
			default:
				if err == nil {
					t.Fatal("injection succeeded")
				}
			}
			got, readErr := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceARN: ip.Summary.ARN})
			if readErr != nil {
				t.Fatal(readErr)
			}
			observed := map[string]string{}
			for _, tag := range got.TagInfoForResource.TagList {
				observed[*tag.Key] = *tag.Value
			}
			if !reflect.DeepEqual(observed, want) {
				t.Fatalf("tags\ngot %+v\nwant %+v", observed, want)
			}
			state, readErr := s.GetIPSet(
				ctx,
				&api.GetIPSetRequest{Name: ip.Summary.Name, Id: ip.Summary.Id, Scope: new("REGIONAL")},
			)
			if readErr != nil {
				t.Fatal(readErr)
			}
			wantAddress := "192.0.2.1/32"
			if failure == "" {
				wantAddress = "198.51.100.1/32"
			}
			if len(state.IPSet.Addresses) != 1 || state.IPSet.Addresses[0] != wantAddress {
				t.Fatalf("configuration compensation %+v", state)
			}
		})
	}
}
