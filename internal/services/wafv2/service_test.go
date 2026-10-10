package wafv2

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
}

func vis() *api.VisibilityConfig {
	return &api.VisibilityConfig{
		SampledRequestsEnabled:   new(false),
		CloudWatchMetricsEnabled: new(true),
		MetricName:               new("metric"),
	}
}

func wantError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || awserror.From(err).Code != code {
		t.Fatalf("wanted %s got %v", code, err)
	}
}

func TestAtomicLockWinnersEveryType(t *testing.T) {
	for _, kind := range []string{"WebACL", "IPSet", "RuleGroup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := testContext()
			s := New(&memory.Store{})
			caller := catalog.NewCaller(api.Descriptor(s))
			create := map[string]any{"Name": "original", "Scope": regional}
			mutable := map[string]any{}
			switch kind {
			case "IPSet":
				create["IPAddressVersion"] = "IPV6"
				create["Addresses"] = []any{"2001:db8::1/128"}
				mutable["Addresses"] = []any{"2001:db8::/64"}
			case "WebACL":
				create["DefaultAction"] = map[string]any{"Allow": map[string]any{}}
				mutable["DefaultAction"] = map[string]any{"Block": map[string]any{}}
				fallthrough
			case "RuleGroup":
				create["VisibilityConfig"] = map[string]any{
					"SampledRequestsEnabled":   false,
					"CloudWatchMetricsEnabled": true,
					"MetricName":               "metric",
				}
				mutable["VisibilityConfig"] = create["VisibilityConfig"]
			}
			if kind == "RuleGroup" {
				create["Capacity"] = float64(10)
			}
			out, err := caller.Call(ctx, serviceName, "Create"+kind, create)
			if err != nil {
				t.Fatal(err)
			}
			summary := out["Summary"].(map[string]any)
			identity := map[string]any{"Name": "original", "Scope": regional, "Id": summary["Id"]}
			desired := map[string]any{}
			for k, v := range identity {
				desired[k] = v
			}
			for k, v := range mutable {
				desired[k] = v
			}
			desired["LockToken"] = summary["LockToken"]
			errors := make(chan error, 12)
			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() { _, e := caller.Call(ctx, serviceName, "Update"+kind, desired); errors <- e })
			}
			wg.Wait()
			close(errors)
			wins := 0
			for e := range errors {
				if e == nil {
					wins++
				} else {
					wantError(t, e, "WAFOptimisticLockException")
				}
			}
			if wins != 1 {
				t.Fatalf("winners=%d", wins)
			}
			_, err = caller.Call(
				ctx,
				serviceName,
				"Delete"+kind,
				map[string]any{
					"Name":      "original",
					"Scope":     regional,
					"Id":        summary["Id"],
					"LockToken": summary["LockToken"],
				},
			)
			wantError(t, err, "WAFOptimisticLockException")
			state, err := caller.Call(ctx, serviceName, "Get"+kind, identity)
			if err != nil {
				t.Fatal(err)
			}
			_, err = caller.Call(
				ctx,
				serviceName,
				"Delete"+kind,
				map[string]any{
					"Name":      "original",
					"Scope":     regional,
					"Id":        summary["Id"],
					"LockToken": state["LockToken"],
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = caller.Call(ctx, serviceName, "Get"+kind, identity)
			wantError(t, err, "WAFNonexistentItemException")
		})
	}
}

func TestCompleteRuleValidationAndReferenceLifecycle(t *testing.T) {
	ctx := testContext()
	s := New(&memory.Store{})
	ip, err := s.CreateIPSet(
		ctx,
		&api.CreateIPSetRequest{
			Name:             new("clients"),
			Scope:            new(regional),
			IPAddressVersion: new("IPV4"),
			Addresses:        []string{"192.0.2.1/32"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	rule := api.Rule{
		Name:             new("direct"),
		Priority:         new(int64(0)),
		Statement:        &api.Statement{IPSetReferenceStatement: &api.IPSetReferenceStatement{ARN: ip.Summary.ARN}},
		Action:           &api.RuleAction{Allow: &api.AllowAction{}},
		VisibilityConfig: vis(),
	}
	group, err := s.CreateRuleGroup(
		ctx,
		&api.CreateRuleGroupRequest{
			Name:             new("group"),
			Scope:            new(regional),
			Capacity:         new(int64(10)),
			Rules:            []api.Rule{rule},
			VisibilityConfig: vis(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetRuleGroup(
		ctx,
		&api.GetRuleGroupRequest{Name: group.Summary.Name, Id: group.Summary.Id, Scope: new(regional)},
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		edit func(*api.Rule)
		name string

		extra bool
	}{
		{name: "duplicate name", edit: func(r *api.Rule) { r.Priority = new(int64(1)) }, extra: true},
		{name: "duplicate priority", edit: func(r *api.Rule) { r.Name = new("another") }, extra: true},
		{name: "missing priority", edit: func(r *api.Rule) { r.Priority = nil }, extra: false},
		{name: "missing statement", edit: func(r *api.Rule) { r.Statement = nil }, extra: false},
		{name: "empty statement", edit: func(r *api.Rule) { r.Statement = &api.Statement{} }, extra: false},
		{name: "missing reference", edit: func(r *api.Rule) {
			r.Statement = &api.Statement{IPSetReferenceStatement: &api.IPSetReferenceStatement{}}
		}, extra: false},
		{name: "two statements", edit: func(r *api.Rule) {
			r.Statement = &api.Statement{
				IPSetReferenceStatement:     &api.IPSetReferenceStatement{ARN: ip.Summary.ARN},
				RuleGroupReferenceStatement: &api.RuleGroupReferenceStatement{ARN: group.Summary.ARN},
			}
		}, extra: false},
		{
			name:  "two actions",
			edit:  func(r *api.Rule) { r.Action = &api.RuleAction{Allow: &api.AllowAction{}, Block: &api.BlockAction{}} },
			extra: false,
		},
		{name: "missing action", edit: func(r *api.Rule) { r.Action = nil }, extra: false},
		{
			name:  "override direct",
			edit:  func(r *api.Rule) { r.OverrideAction = &api.OverrideAction{None: &api.NoneAction{}} },
			extra: false,
		},
		{name: "group nested", edit: func(r *api.Rule) {
			r.Statement = &api.Statement{
				RuleGroupReferenceStatement: &api.RuleGroupReferenceStatement{ARN: group.Summary.ARN},
			}
			r.Action = nil
			r.OverrideAction = &api.OverrideAction{Count: &api.CountAction{}}
		}, extra: false},
		{name: "missing visibility", edit: func(r *api.Rule) { r.VisibilityConfig = nil }, extra: false},
		{name: "metric length", edit: func(r *api.Rule) {
			r.VisibilityConfig = &api.VisibilityConfig{
				SampledRequestsEnabled:   new(false),
				CloudWatchMetricsEnabled: new(false),
				MetricName:               new(fmt.Sprintf("%0256d", 0)),
			}
		}, extra: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			changed := rule
			test.edit(&changed)
			rules := []api.Rule{changed}
			if test.extra {
				rules = append(rules, rule)
			}
			_, err = s.UpdateRuleGroup(
				ctx,
				&api.UpdateRuleGroupRequest{
					Name:             group.Summary.Name,
					Id:               group.Summary.Id,
					Scope:            new(regional),
					LockToken:        before.LockToken,
					Description:      new("Invalid changed configuration"),
					Rules:            rules,
					VisibilityConfig: vis(),
				},
			)
			wantError(t, err, "WAFInvalidParameterException")
			after, e := s.GetRuleGroup(
				ctx,
				&api.GetRuleGroupRequest{Name: group.Summary.Name, Id: group.Summary.Id, Scope: new(regional)},
			)
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid update changed state %+v %v", after, e)
			}
		})
	}
	_, err = s.DeleteIPSet(
		ctx,
		&api.DeleteIPSetRequest{
			Name:      ip.Summary.Name,
			Id:        ip.Summary.Id,
			Scope:     new(regional),
			LockToken: ip.Summary.LockToken,
		},
	)
	wantError(t, err, "WAFAssociatedItemException")
	for _, cap := range []*int64{nil, new(int64(0)), new(int64(5001))} {
		_, err = s.CreateRuleGroup(
			ctx,
			&api.CreateRuleGroupRequest{
				Name:             new("capacity"),
				Scope:            new(regional),
				Capacity:         cap,
				VisibilityConfig: vis(),
			},
		)
		wantError(t, err, "WAFInvalidParameterException")
	}
	// Invalid reference types and unknown/foreign exact references cannot publish.
	badRefs := []string{
		*group.Summary.ARN,
		"arn:aws:wafv2:us-east-1:000000000000:regional/ipset/missing/00000000-0000-0000-0000-000000000000",
		"arn:aws:wafv2:eu-west-1:000000000000:regional/ipset/clients/" + *ip.Summary.Id,
	}
	for _, ref := range badRefs {
		changed := rule
		changed.Statement = &api.Statement{IPSetReferenceStatement: &api.IPSetReferenceStatement{ARN: new(ref)}}
		_, err = s.UpdateRuleGroup(
			ctx,
			&api.UpdateRuleGroupRequest{
				Name:             group.Summary.Name,
				Id:               group.Summary.Id,
				Scope:            new(regional),
				LockToken:        before.LockToken,
				Rules:            []api.Rule{changed},
				VisibilityConfig: vis(),
			},
		)
		code := "WAFNonexistentItemException"
		if ref == *group.Summary.ARN {
			code = "WAFInvalidParameterException"
		}
		wantError(t, err, code)
	}
}

func TestCIDRFamilyAndLimits(t *testing.T) {
	for _, test := range []struct {
		family string

		code      string
		addresses []string
	}{
		{family: "IPV4", addresses: []string{"192.0.2.1/1", "192.0.2.1/32"}, code: ""},
		{family: "IPV6", addresses: []string{
			"2001:db8::1/1", "2001:db8::1/128", "1111:0000:0000:0000:0000:0000:0000:0111/128",
		}, code: ""},
		{family: "IPV4", addresses: []string{}, code: ""},
		{family: "IPV6", addresses: []string{}, code: ""},
		{family: "IPV4", addresses: nil, code: "WAFInvalidParameterException"},
		{family: "IPV4", addresses: []string{"2001:db8::1/128"}, code: "WAFInvalidParameterException"},
		{family: "IPV6", addresses: []string{"192.0.2.1/32"}, code: "WAFInvalidParameterException"},
		{family: "IPV4", addresses: make([]string, 10001), code: "WAFLimitsExceededException"},
	} {
		err := addressesOK(test.family, test.addresses)
		if test.code == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			wantError(t, err, test.code)
		}
	}
}
