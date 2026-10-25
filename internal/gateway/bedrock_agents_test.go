package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	bedrockapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	agentapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	runtimeapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	inferenceapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/config"
)

// Every operation must survive the shared bedrock signing-name dispatch, regardless of service registration order.
func TestBedrockAgentOperationRouting(t *testing.T) {
	descriptors := []catalog.Service{
		bedrockapi.Descriptor(nil),
		agentapi.Descriptor(nil),
		runtimeapi.Descriptor(nil),
		inferenceapi.Descriptor(nil),
	}

	labels := regexp.MustCompile(`\{[^}]+\}`)
	routesSeen := map[string]string{}
	for i := range descriptors {
		service := &descriptors[i]
		for name, operation := range service.Operations {
			binding := operation.Bindings[catalog.RESTJSON]
			route := binding.Method + " " + labels.ReplaceAllString(binding.URI, "{label}")
			if previous, ok := routesSeen[route]; ok {
				t.Fatalf("route collision: %s and %s.%s: %s", previous, service.Name, name, route)
			}

			routesSeen[route] = service.Name + "." + name
			marker := service.Name + "." + name
			operation.Invoke = func(context.Context, any) (any, error) {
				return nil, awserror.New("ValidationException", marker)
			}

			service.Operations[name] = operation
		}
	}

	for _, reverse := range []bool{false, true} {
		if reverse {
			for i, j := 0, len(descriptors)-1; i < j; i, j = i+1, j-1 {
				descriptors[i], descriptors[j] = descriptors[j], descriptors[i]
			}
		}

		gateway, err := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), descriptors...)
		if err != nil {
			t.Fatal(err)
		}

		for _, service := range descriptors {
			for name, operation := range service.Operations {
				t.Run(fmt.Sprintf("%t/%s/%s", reverse, service.Name, name), func(t *testing.T) {
					binding := operation.Bindings[catalog.RESTJSON]
					path := labels.ReplaceAllString(binding.URI, "abcdefghij")
					req := httptest.NewRequest(binding.Method, path, strings.NewReader(`{}`))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set(
						"Authorization",
						"AWS4-HMAC-SHA256 Credential=test/20261025/us-east-1/bedrock/aws4_request, "+
							"SignedHeaders=host;x-amz-date, Signature=unused",
					)
					req.Header.Set("X-Amz-Date", "20261025T000000Z")
					out := httptest.NewRecorder()
					gateway.ServeHTTP(out, req)
					if !strings.Contains(out.Body.String(), service.Name+"."+name) {
						t.Fatalf("wrong dispatch: %d %s", out.Code, out.Body.String())
					}
				})
			}
		}
	}
}
