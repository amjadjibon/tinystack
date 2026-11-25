package sns

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const attrDataProtectionPolicy = "DataProtectionPolicy"

type dataProtectionPolicyDoc struct {
	Configuration map[string]any               `json:"Configuration,omitempty"`
	Name          string                       `json:"Name,omitempty"`
	Description   string                       `json:"Description,omitempty"`
	Version       string                       `json:"Version"`
	Statement     []dataProtectionStatementDoc `json:"Statement"`
}

type dataProtectionStatementDoc struct {
	Operation      map[string]json.RawMessage `json:"Operation"`
	Sid            string                     `json:"Sid,omitempty"`
	DataDirection  string                     `json:"DataDirection"`
	Principal      json.RawMessage            `json:"Principal"`
	DataIdentifier []string                   `json:"DataIdentifier"`
}

// validateDataProtectionPolicy checks the AWS SNS data protection policy JSON
// structure. TinyStack stores and returns the policy without performing PII
// scanning or redaction.
func validateDataProtectionPolicy(raw string, allowClear bool) (string, error) {
	if allowClear && (raw == "" || raw == "{}") {
		return "", nil
	}

	if raw == "" || len(raw) > maxPolicy {
		return "", invalid("DataProtectionPolicy must be a valid JSON document of at most %d bytes", maxPolicy)
	}

	var doc dataProtectionPolicyDoc
	if err := decodeStrictJSON(raw, &doc); err != nil {
		return "", invalid("DataProtectionPolicy: %s", err)
	}

	if doc.Version != "2021-06-01" {
		return "", invalid("DataProtectionPolicy: Version must be 2021-06-01")
	}

	if len(doc.Statement) == 0 {
		return "", invalid("DataProtectionPolicy: Statement must contain at least one statement")
	}

	for _, st := range doc.Statement {
		if st.DataDirection != "Inbound" && st.DataDirection != "Outbound" {
			return "", invalid("DataProtectionPolicy: DataDirection must be Inbound or Outbound")
		}

		if err := validateDataProtectionPrincipal(st.Principal); err != nil {
			return "", err
		}

		if len(st.DataIdentifier) == 0 {
			return "", invalid("DataProtectionPolicy: DataIdentifier must contain at least one identifier")
		}

		for _, id := range st.DataIdentifier {
			if id == "" {
				return "", invalid("DataProtectionPolicy: DataIdentifier entries must be non-empty")
			}
		}

		if err := validateDataProtectionOperation(st.Operation); err != nil {
			return "", err
		}
	}

	return raw, nil
}

func validateDataProtectionPrincipal(raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid("DataProtectionPolicy: Principal is required")
	}

	var single string
	if json.Unmarshal(raw, &single) == nil {
		if single == "" {
			return invalid("DataProtectionPolicy: Principal must be non-empty")
		}

		return nil
	}

	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return invalid("DataProtectionPolicy: Principal must be a non-empty string or list of strings")
	}

	for _, p := range list {
		if p == "" {
			return invalid("DataProtectionPolicy: Principal entries must be non-empty")
		}
	}

	return nil
}

func validateDataProtectionOperation(op map[string]json.RawMessage) error {
	if len(op) == 0 {
		return invalid("DataProtectionPolicy: Operation must specify Audit, Deidentify, or Deny")
	}

	for k, v := range op {
		switch k {
		case "Audit", "Deidentify", "Deny":
			var obj map[string]any
			if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
				return invalid("DataProtectionPolicy: Operation.%s must be a JSON object", k)
			}
		default:
			return invalid("DataProtectionPolicy: Operation %q is not supported", k)
		}
	}

	return nil
}

func (s *Service) GetDataProtectionPolicy(
	ctx context.Context,
	in *api.GetDataProtectionPolicyInput,
) (*api.GetDataProtectionPolicyResponse, error) {
	key, _, err := resolve(ctx, in.ResourceArn, false)
	if err != nil {
		return nil, err
	}

	var policy string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		policy = t.DataProtectionPolicy
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetDataProtectionPolicyResponse{}
	if policy != "" {
		out.DataProtectionPolicy = new(policy)
	}

	return out, nil
}

func (s *Service) PutDataProtectionPolicy(
	ctx context.Context,
	in *api.PutDataProtectionPolicyInput,
) (*api.PutDataProtectionPolicyOutput, error) {
	key, _, err := resolve(ctx, in.ResourceArn, false)
	if err != nil {
		return nil, err
	}

	raw := common.Value(in.DataProtectionPolicy)
	if raw == "" {
		return nil, invalid("DataProtectionPolicy is required")
	}

	policy, err := validateDataProtectionPolicy(raw, true)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if t.fifo() {
			return invalid("DataProtectionPolicy is only supported for standard topics")
		}

		t.DataProtectionPolicy = policy
		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutDataProtectionPolicyOutput{}, nil
}
