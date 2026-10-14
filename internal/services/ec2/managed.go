package ec2

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// RunningInstance is a running instance, as other services see it.
type RunningInstance struct {
	Tags map[string]string
	ID   string
}

// RunningInstances lists the running instances of scope, for services that
// act on instances, such as Systems Manager's Run Command.
func (s *Service) RunningInstances(ctx context.Context, scope request.Scope) ([]RunningInstance, error) {
	var out []RunningInstance
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		all, err := listInstances(tx, scope, s.now())
		if err != nil {
			return err
		}

		for _, i := range all {
			if i.State == stateRunning {
				out = append(out, RunningInstance{ID: i.ID, Tags: maps.Clone(i.Tags)})
			}
		}

		return nil
	})

	return out, err
}
