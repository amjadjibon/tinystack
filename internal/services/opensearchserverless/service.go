// Package opensearchserverless implements records-only collection and policy management.
package opensearchserverless

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "opensearchserverless"
	collectionKind = "collection"
	encryptionType = "encryption"
	networkType    = "network"
	dataType       = "data"
)

var (
	collectionNamePattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	namePattern                   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
	_                     api.API = (*Service)(nil)
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }
func missing() error                   { return awserror.New("ResourceNotFoundException", "Resource does not exist.") }

func conflict() error {
	return awserror.New("ConflictException", "Resource already exists or policy version conflicts.")
}

type tokenRecord struct {
	Hash   string
	Output json.RawMessage
}

// transact atomically records idempotency results alongside the resource change.
func transact[T any](ctx context.Context, s *Service, operation string, token *string, input any,
	change func(storage.WriteTx, request.Scope) (*T, error),
) (*T, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	key := ""
	if token != nil {
		if len(*token) < 1 || len(*token) > 512 {
			return nil, awserror.Validation("Invalid clientToken.")
		}

		key = scope.Key(serviceName, "token", operation+"/"+fmt.Sprintf("%x", sha256.Sum256([]byte(*token))))
	}

	var out *T
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if key != "" {
			r, found, e := storage.Read[tokenRecord](tx, key)
			if e != nil {
				return e
			}

			if found {
				if r.Hash != hash {
					return conflict()
				}

				out = new(T)
				return json.Unmarshal(r.Output, out)
			}
		}

		var e error
		out, e = change(tx, scope)
		if e != nil {
			return e
		}

		if key == "" {
			return nil
		}

		data, e := json.Marshal(out)
		if e != nil {
			return e
		}

		return storage.Write(tx, key, tokenRecord{Hash: hash, Output: data})
	})
	return out, err
}

type policy struct {
	Type             *string `json:"type"`
	Name             *string `json:"name"`
	PolicyVersion    *string `json:"policyVersion"`
	Description      *string `json:"description,omitempty"`
	Policy           any     `json:"policy"`
	CreatedDate      *int64  `json:"createdDate"`
	LastModifiedDate *int64  `json:"lastModifiedDate"`
}

type rule struct {
	ResourceType string   `json:"ResourceType"`
	Resource     []string `json:"Resource"`
	Permission   []string `json:"Permission,omitempty"`
}

type policyDocument struct {
	Rules           []rule   `json:"Rules"`
	AWSOwnedKey     *bool    `json:"AWSOwnedKey,omitempty"`
	KmsARN          string   `json:"KmsARN,omitempty"`
	Description     string   `json:"Description,omitempty"`
	AllowFromPublic *bool    `json:"AllowFromPublic,omitempty"`
	SourceVPCEs     []string `json:"SourceVPCEs,omitempty"`
	SourceServices  []string `json:"SourceServices,omitempty"`
	Principal       []string `json:"Principal,omitempty"`
}

func decodePolicy(kind string, text *string) (any, error) {
	raw := []byte(common.Value(text))
	if len(raw) < 1 || len(raw) > 20480 || !json.Valid(raw) {
		return nil, awserror.Validation("Policy must contain valid JSON of at most 20480 bytes.")
	}

	var docs []policyDocument
	if kind == encryptionType {
		var d policyDocument
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			return nil, awserror.Validation("Invalid encryption policy: %s", err)
		}

		docs = []policyDocument{d}
	} else {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&docs); err != nil {
			return nil, awserror.Validation("Invalid policy: %s", err)
		}
	}

	if len(docs) == 0 {
		return nil, awserror.Validation("Policy must contain rules.")
	}

	for i := range docs {
		d := &docs[i]
		if len(d.Rules) == 0 {
			return nil, awserror.Validation("Policy must contain rules.")
		}

		switch kind {
		case encryptionType:
			if d.AllowFromPublic != nil || d.SourceVPCEs != nil || d.SourceServices != nil || d.Principal != nil ||
				d.Description != "" {
				return nil, awserror.Validation("Encryption policy contains unsupported fields.")
			}

			if (d.AWSOwnedKey == nil || !*d.AWSOwnedKey) && !strings.HasPrefix(d.KmsARN, "arn:aws:kms:") {
				return nil, awserror.Validation("Encryption requires AWSOwnedKey or KmsARN.")
			}

		case networkType:
			if d.AWSOwnedKey != nil || d.KmsARN != "" || d.Principal != nil {
				return nil, awserror.Validation("Network policy contains unsupported fields.")
			}

			for _, source := range d.SourceVPCEs {
				if !regexp.MustCompile(`^vpce-[a-z0-9]+$`).MatchString(source) {
					return nil, awserror.Validation("Invalid VPC endpoint source.")
				}
			}

			for _, source := range d.SourceServices {
				if source != "bedrock.amazonaws.com" {
					return nil, awserror.Validation("Invalid service source.")
				}
			}

			if d.AllowFromPublic == nil && len(d.SourceVPCEs) == 0 && len(d.SourceServices) == 0 {
				return nil, awserror.Validation("Network policy requires a source.")
			}

		case dataType:
			if d.AWSOwnedKey != nil || d.KmsARN != "" || d.AllowFromPublic != nil || d.SourceVPCEs != nil ||
				d.SourceServices != nil {
				return nil, awserror.Validation("Data policy contains unsupported fields.")
			}

			if len(d.Principal) == 0 {
				return nil, awserror.Validation("Data policy requires Principal.")
			}

			for _, principal := range d.Principal {
				if !strings.HasPrefix(principal, "arn:aws:iam::") && !strings.HasPrefix(principal, "saml/") {
					return nil, awserror.Validation("Invalid principal.")
				}
			}
		}

		for _, r := range d.Rules {
			allowed := []string{collectionKind}
			if kind == networkType {
				allowed = append(allowed, "dashboard")
			}

			if kind == dataType {
				allowed = append(allowed, "index")
			}

			if !slices.Contains(allowed, r.ResourceType) || len(r.Resource) == 0 {
				return nil, awserror.Validation("Invalid policy resource type or empty resources.")
			}

			for _, resource := range r.Resource {
				prefix := r.ResourceType
				if prefix == "dashboard" {
					prefix = collectionKind
				}

				validPattern := regexp.MustCompile(`^collection/(?:[a-z][a-z0-9-]*\*?|\*)$`)
				if prefix == "index" {
					validPattern = regexp.MustCompile(`^index/(?:[a-z][a-z0-9-]*\*?|\*)/[^/]+$`)
				}

				if !validPattern.MatchString(resource) {
					return nil, awserror.Validation("Invalid policy resource.")
				}
			}

			if kind == dataType {
				if len(r.Permission) == 0 {
					return nil, awserror.Validation("Data rules require Permission.")
				}

				allowedPermissions := []string{
					"aoss:*", "aoss:CreateCollectionItems", "aoss:DeleteCollectionItems",
					"aoss:UpdateCollectionItems", "aoss:DescribeCollectionItems",
				}

				if r.ResourceType == "index" {
					allowedPermissions = []string{
						"aoss:*", "aoss:CreateIndex", "aoss:DeleteIndex",
						"aoss:UpdateIndex", "aoss:DescribeIndex", "aoss:ReadDocument", "aoss:WriteDocument",
					}
				}

				for _, permission := range r.Permission {
					if !slices.Contains(allowedPermissions, permission) {
						return nil, awserror.Validation("Invalid permission.")
					}
				}
			} else if len(r.Permission) > 0 {
				return nil, awserror.Validation("Permissions require a data policy.")
			}
		}
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, awserror.Validation("Invalid JSON policy.")
	}

	return value, nil
}

func policyKey(scope request.Scope, family, kind, name string) string {
	return scope.Key(serviceName, family, kind+"/"+name)
}

func policyType(family string, kind *string) error {
	allowed := []string{encryptionType, networkType}
	if family == "access" {
		allowed = []string{dataType}
	}

	if !slices.Contains(allowed, common.Value(kind)) {
		return awserror.Validation("Invalid policy type.")
	}

	return nil
}

func readPolicy(tx storage.ReadTx, scope request.Scope, family string, kind, name *string) (*policy, error) {
	if err := policyType(family, kind); err != nil {
		return nil, err
	}

	r, found, err := storage.Read[policy](tx, policyKey(scope, family, common.Value(kind), common.Value(name)))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func matchesPolicy(p *policy, resource string) bool { return policySpecificity(p, resource) >= 0 }

// The most specific matching encryption rule selects the collection's immutable key.
func policySpecificity(p *policy, resource string) int {
	raw, err := json.Marshal(p.Policy)
	if err != nil {
		return -1
	}

	var docs []policyDocument
	var single policyDocument
	if json.Unmarshal(raw, &single) == nil {
		docs = []policyDocument{single}
	} else if json.Unmarshal(raw, &docs) != nil {
		return -1
	}

	score := -1
	for i := range docs {
		for _, r := range docs[i].Rules {
			for _, pattern := range r.Resource {
				if matched, _ := path.Match(pattern, resource); matched {
					candidate := len(strings.TrimSuffix(pattern, "*"))
					if !strings.Contains(pattern, "*") {
						candidate += 10000
					}

					score = max(score, candidate)
				}
			}
		}
	}

	return score
}

func (s *Service) writePolicy(ctx context.Context, family string, kind, name, description, text, version, token *string,
	input any, update bool,
) (*policy, error) {
	if err := policyType(family, kind); err != nil {
		return nil, err
	}

	if !namePattern.MatchString(common.Value(name)) {
		return nil, awserror.Validation("Invalid policy name.")
	}

	if len(common.Value(description)) > 1000 {
		return nil, awserror.Validation("Description exceeds 1000 characters.")
	}

	var doc any
	if text != nil || !update {
		var err error
		doc, err = decodePolicy(common.Value(kind), text)
		if err != nil {
			return nil, err
		}
	}

	operation := "Create"
	if update {
		operation = "Update"
	}

	return transact(
		ctx,
		s,
		operation+family,
		token,
		input,
		func(tx storage.WriteTx, scope request.Scope) (*policy, error) {
			key := policyKey(scope, family, *kind, *name)
			old, found, err := storage.Read[policy](tx, key)
			if err != nil {
				return nil, err
			}

			now := time.Now().UnixMilli()
			if !update {
				if found {
					return nil, conflict()
				}

				old = &policy{Type: kind, Name: name, CreatedDate: new(now), PolicyVersion: new(common.RandomID(24))}
			} else {
				if !found {
					return nil, missing()
				}

				if version == nil || common.Value(old.PolicyVersion) != *version {
					return nil, conflict()
				}

				old.PolicyVersion = new(common.RandomID(24))
			}

			if description != nil {
				old.Description = description
			}

			if text != nil {
				old.Policy = doc
			}

			old.LastModifiedDate = new(now)
			return old, storage.Write(tx, key, old)
		},
	)
}
