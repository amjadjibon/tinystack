package sfn

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every state machine and activity, for the resource
// browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case ok && strings.HasPrefix(rest, kindMachine+"/"):
				sm, _, err := read[stateMachine](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "state-machine", Account: account, Region: region, Name: sm.Name,
					ARN: arn(scope, arnStateMachine, sm.Name), Tags: sm.Tags,
					Details: map[string]any{"Type": sm.Type, "RoleArn": sm.RoleArn},
				})
			case ok && strings.HasPrefix(rest, kindActivity+"/"):
				a, _, err := read[activity](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: kindActivity, Account: account, Region: region, Name: a.Name,
					ARN: arn(scope, kindActivity, a.Name), Tags: a.Tags,
				})
			}
		}

		return nil
	})

	return out, err
}
