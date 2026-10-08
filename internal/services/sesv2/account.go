package sesv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// maxPerDay and maxRate are the sending quota GetAccount reports, which
	// TinyStack does not enforce.
	maxPerDay = 50000
	maxRate   = 14
)

// sending counts the messages a Region sent, by Unix hour.
type sending struct {
	Hours map[int64]int64
}

func sendingKey(scope request.Scope) string { return scope.Key(service, kindAccount, "sending") }

// count adds n messages to the current hour, forgetting hours older than a
// day.
func (s *Service) count(tx storage.WriteTx, scope request.Scope, n int) error {
	c, _, err := storage.Read[sending](tx, sendingKey(scope))
	if err != nil {
		return err
	}

	if c == nil {
		c = &sending{}
	}

	hour := s.now().Unix() / 3600
	if c.Hours == nil {
		c.Hours = map[int64]int64{}
	}

	for h := range c.Hours {
		if h <= hour-24 {
			delete(c.Hours, h)
		}
	}

	c.Hours[hour] += int64(n)
	return storage.Write(tx, sendingKey(scope), c)
}

func (s *Service) GetAccount(ctx context.Context, _ *api.GetAccountRequest) (*api.GetAccountResponse, error) {
	sent := 0.0
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, _, err := storage.Read[sending](tx, sendingKey(scope))
		hour := s.now().Unix() / 3600
		if c != nil {
			for h, n := range c.Hours {
				if h > hour-24 {
					sent += float64(n)
				}
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAccountResponse{
		DedicatedIpAutoWarmupEnabled: new(false),
		EnforcementStatus:            new("HEALTHY"),
		ProductionAccessEnabled:      new(true),
		SendingEnabled:               new(true),
		SendQuota: &api.SendQuota{
			Max24HourSend:   new(float64(maxPerDay)),
			MaxSendRate:     new(float64(maxRate)),
			SentLast24Hours: new(sent),
		},
	}, nil
}

// SentByHour returns how many messages the caller's Region sent in each of the
// last 24 hours, keyed by the Unix hour, for API v1's GetSendStatistics.
func (s *Service) SentByHour(ctx context.Context) (map[int64]int64, error) {
	out := map[int64]int64{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, _, err := storage.Read[sending](tx, sendingKey(scope))
		if c != nil {
			hour := s.now().Unix() / 3600
			for h, n := range c.Hours {
				if h > hour-24 {
					out[h] = n
				}
			}
		}

		return err
	})

	return out, err
}
