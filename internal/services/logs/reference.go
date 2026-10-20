package logs

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CheckStreamARN validates a local log stream reference in the caller's transaction.
func (s *Service) CheckStreamARN(tx storage.ReadTx, scope request.Scope, arn string) error {
	prefix := "arn:aws:logs:" + scope.Region + ":" + scope.AccountID + ":log-group:"
	path, ok := strings.CutPrefix(arn, prefix)
	if !ok {
		return invalid("Log stream ARN must belong to this account and Region.")
	}

	group, name, ok := strings.Cut(path, ":log-stream:")
	if !ok || group == "" || name == "" {
		return invalid("Invalid log stream ARN.")
	}

	_, err := loadStream(tx, scope, group, name)
	return err
}
