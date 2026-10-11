package mwaa

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) ListEnvironments(
	ctx context.Context,
	in *api.ListEnvironmentsInput,
) (*api.ListEnvironmentsOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	limit := int64(25)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}
	if limit < 1 || limit > 25 {
		return nil, invalid("MaxResults must be 1-25 (local default 25)")
	}
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/mwaa/ListEnvironments"))
	prefix := fmt.Sprintf("%x:", sum)
	after := ""
	if in.NextToken != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			return nil, invalid("Invalid NextToken for this scope and API")
		}
		after = string(raw[len(prefix):])
		if !namePattern.MatchString(after) {
			return nil, invalid("Invalid NextToken cursor")
		}
	}
	var names []string
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, err := storage.Records[record](tx, scope.Prefix(serviceName, environmentKind))
		if err != nil {
			return err
		}
		for _, r := range rows {
			names = append(names, *r.Environment.Name)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	slices.Sort(names)
	out := &api.ListEnvironmentsOutput{Environments: []string{}}
	for _, name := range names {
		if name <= after {
			continue
		}
		if int64(len(out.Environments)) == limit {
			last := out.Environments[len(out.Environments)-1]
			out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(prefix + last)))
			break
		}
		out.Environments = append(out.Environments, name)
	}
	return common.Clone(out), nil
}
