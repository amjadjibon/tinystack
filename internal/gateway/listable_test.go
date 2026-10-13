package gateway

import (
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestListAction(t *testing.T) {
	service := &catalog.Service{IAMPrefix: "demo", Operations: map[string]catalog.Operation{
		"ListRoles":                 {Name: "ListRoles"},
		"GetRole":                   {Name: "GetRole"},
		"ListBuckets":               {Name: "ListBuckets", IAMAction: "demo:ListAllMyBuckets"},
		"DescribeAddresses":         {Name: "DescribeAddresses"},
		"ListPolicies":              {Name: "ListPolicies"},
		"GetApis":                   {Name: "GetApis"},
		"DescribeReplicationGroups": {Name: "DescribeReplicationGroups"},
	}}

	for kind, want := range map[string]string{
		"role":              "demo:ListRoles",
		"bucket":            "demo:ListAllMyBuckets",
		"address":           "demo:DescribeAddresses",
		"policy":            "demo:ListPolicies",
		"api":               "demo:GetApis",
		"replication-group": "demo:DescribeReplicationGroups",
		"queue":             "",
	} {
		if got := listAction(service, kind); got != want {
			t.Errorf("listAction(%q) = %q, want %q", kind, got, want)
		}
	}
}
