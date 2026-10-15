//go:build console

package gateway

import (
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	cognitoidentityapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	elasticacheapi "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
)

// Every operation the shell keeps for administrators must exist, or the entry
// guards nothing.
func TestShellAdminOnlyNamesRealOperations(t *testing.T) {
	services := map[string]catalog.Service{
		"lambda": lambdaapi.Descriptor(nil), "ecs": ecsapi.Descriptor(nil), "eks": eksapi.Descriptor(nil),
		"elasticache": elasticacheapi.Descriptor(nil), "rds": rdsapi.Descriptor(nil), "sts": stsapi.Descriptor(nil),
		"iam": iamapi.Descriptor(nil), "cognitoidentity": cognitoidentityapi.Descriptor(nil),
	}
	if len(services) != len(shellAdminOnly) {
		t.Fatalf("services = %d, shellAdminOnly = %d", len(services), len(shellAdminOnly))
	}

	for name, operations := range shellAdminOnly {
		for _, operation := range operations {
			if _, ok := services[name].Operations[operation]; !ok {
				t.Errorf("%s has no %s operation", name, operation)
			}
		}
	}
}
