package location

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) ListTrackers(ctx context.Context, in *api.ListTrackersRequest) (*api.ListTrackersResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ListTrackers input is required")
	}
	limit := int64(100)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}
	if limit < 1 || limit > 100 {
		return nil, invalid("MaxResults requires 1-100; default 100")
	}
	prefix := fmt.Sprintf("%x:", sha256.Sum256([]byte(scope.AccountID+"/"+scope.Region+"/location/ListTrackers")))
	after, incarnation := "", ""
	if in.NextToken != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if err != nil || len(*in.NextToken) > 2000 || !strings.HasPrefix(string(raw), prefix) {
			return nil, invalid("Invalid NextToken for this scope and API")
		}
		parts := strings.Split(string(raw[len(prefix):]), ":")
		if len(parts) != 2 || !namePattern.MatchString(parts[0]) {
			return nil, invalid("Invalid tracker cursor")
		}
		if _, err = uuid.Parse(parts[1]); err != nil {
			return nil, invalid("Invalid tracker cursor incarnation")
		}
		after, incarnation = parts[0], parts[1]
	}
	out := &api.ListTrackersResponse{Entries: []api.ListTrackersResponseEntry{}}
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		// Deleted cursor records still resume by name; a recreated cursor is a new incarnation.
		if after != "" {
			r, ok, err := storage.Read[Snapshot](tx, scope.Key(serviceName, trackerKind, after))
			if err != nil {
				return err
			}
			if ok && r.Incarnation != incarnation {
				return invalid("NextToken cursor tracker was recreated")
			}
		}
		keys := tx.Keys(scope.Prefix(serviceName, trackerKind))
		slices.Sort(keys)
		last := ""
		for _, key := range keys {
			r, _, err := storage.Read[Snapshot](tx, key)
			if err != nil {
				return err
			}
			t := r.Tracker
			if *t.TrackerName <= after {
				continue
			}
			if int64(len(out.Entries)) == limit {
				out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(prefix + last)))
				break
			}
			out.Entries = append(
				out.Entries,
				api.ListTrackersResponseEntry{
					TrackerName: t.TrackerName,
					Description: t.Description,
					CreateTime:  t.CreateTime,
					UpdateTime:  t.UpdateTime,
				},
			)
			last = *t.TrackerName + ":" + r.Incarnation
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(out), nil
}
