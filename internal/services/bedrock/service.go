// Package bedrock implements a fixed model catalogue and local control-plane records.
package bedrock

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "bedrock"

var (
	_            api.API = (*Service)(nil)
	resourceName         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$`)
)

// Service stores account- and Region-scoped control-plane records.
type Service struct{ store storage.Store }

// New constructs the control plane.
func New(store storage.Store) *Service { return &Service{store: store} }

func invalid(message string) error { return awserror.New("ValidationException", message) }
func missing() error               { return awserror.New("ResourceNotFoundException", "Resource does not exist.") }

func resourceARN(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func resourceID(scope request.Scope, kind, id string) (string, error) {
	if strings.HasPrefix(id, "arn:") {
		rest, ok := strings.CutPrefix(id, resourceARN(scope, kind, ""))
		if !ok {
			return "", missing()
		}

		id = rest
	}

	if id == "" || strings.ContainsAny(id, "/\\") {
		return "", invalid("Invalid resource identifier.")
	}

	return id, nil
}

func pageSize(n *int64) (int, error) {
	if n == nil {
		return 100, nil
	}

	if *n < 1 || *n > 100 {
		return 0, invalid("maxResults must be between 1 and 100.")
	}

	return int(*n), nil
}

// Resolve returns the foundation model behind an ID or a caller-scoped profile ARN.
func (s *Service) Resolve(ctx context.Context, id string) (*Model, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(id, "arn:") {
		prefix := fmt.Sprintf("arn:aws:bedrock:%s::foundation-model/", scope.Region)
		if modelID, ok := strings.CutPrefix(id, prefix); ok {
			return findModel(modelID)
		}
	} else {
		for i := range models {
			model := &models[i]
			if model.ID == id {
				return common.Clone(model), nil
			}
		}
	}

	profile, err := s.GetInferenceProfile(ctx, &api.GetInferenceProfileRequest{InferenceProfileIdentifier: new(id)})
	if err != nil {
		return nil, err
	}

	if len(profile.Models) == 0 {
		return nil, missing()
	}

	_, modelID, ok := strings.Cut(common.Value(profile.Models[0].ModelArn), "foundation-model/")
	if !ok {
		return nil, missing()
	}

	return findModel(modelID)
}

const (
	guardrailKind          = "guardrail"
	applicationProfileKind = "application-inference-profile"
	providerAnthropic      = "Anthropic"
	familyAnthropic        = "anthropic"
	providerAmazon         = "Amazon"
	providerMeta           = "Meta"
	modalityImage          = "IMAGE"
	modalityVideo          = "VIDEO"
	inferenceOnDemand      = "ON_DEMAND"
	inferenceProvisioned   = "PROVISIONED"
	familyNova             = "nova"
	familyLlama            = "llama"
)

const modalityText = "TEXT"
