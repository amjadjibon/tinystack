package cloudfront

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type oacRecord struct {
	OAC  *api.OriginAccessControl `json:"oac"`
	ETag string                   `json:"etag"`
}

func oacLocation(id string) string {
	return "https://cloudfront.amazonaws.com/2020-05-31/origin-access-control/" + id
}

func toOACSummary(rec *oacRecord) api.OriginAccessControlSummary {
	if rec == nil || rec.OAC == nil {
		return api.OriginAccessControlSummary{}
	}
	cfg := rec.OAC.OriginAccessControlConfig
	s := api.OriginAccessControlSummary{
		Id: rec.OAC.Id,
	}
	if cfg != nil {
		s.Description = cfg.Description
		s.Name = cfg.Name
		s.OriginAccessControlOriginType = cfg.OriginAccessControlOriginType
		s.SigningBehavior = cfg.SigningBehavior
		s.SigningProtocol = cfg.SigningProtocol
	}
	return s
}

func (s *Service) CreateOriginAccessControl(
	ctx context.Context, in *api.CreateOriginAccessControlRequest,
) (*api.CreateOriginAccessControlResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.OriginAccessControlConfig == nil {
		return nil, invalidArgument("OriginAccessControlConfig is required")
	}

	name := common.Value(in.OriginAccessControlConfig.Name)
	if name == "" {
		return nil, invalidArgument("Name is required")
	}

	var created *oacRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// Check name uniqueness
		if _, exists := tx.Get(key(m.AccountID, kindOACName, name)); exists {
			return originAccessControlAlreadyExists()
		}

		var id string
		for {
			id = newID("E")
			if _, exists := tx.Get(key(m.AccountID, kindOAC, id)); !exists {
				break
			}
		}

		etag := newID("E")
		oac := &api.OriginAccessControl{
			Id:                        &id,
			OriginAccessControlConfig: in.OriginAccessControlConfig,
		}
		rec := &oacRecord{
			OAC:  oac,
			ETag: etag,
		}

		if writeErr := storage.Write(tx, key(m.AccountID, kindOAC, id), rec); writeErr != nil {
			return writeErr
		}
		tx.Put(key(m.AccountID, kindOACName, name), []byte(id))

		created = rec
		return nil
	})
	if err != nil {
		return nil, err
	}

	loc := oacLocation(common.Value(created.OAC.Id))
	return &api.CreateOriginAccessControlResult{
		OriginAccessControl: created.OAC,
		ETag:                &created.ETag,
		Location:            &loc,
	}, nil
}

func (s *Service) GetOriginAccessControl(
	ctx context.Context, in *api.GetOriginAccessControlRequest,
) (*api.GetOriginAccessControlResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.Id)
	if id == "" {
		return nil, noSuchOriginAccessControl("")
	}

	var rec *oacRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[oacRecord](tx, key(m.AccountID, kindOAC, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchOriginAccessControl(id)
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetOriginAccessControlResult{
		OriginAccessControl: rec.OAC,
		ETag:                &rec.ETag,
	}, nil
}

func (s *Service) GetOriginAccessControlConfig(
	ctx context.Context, in *api.GetOriginAccessControlConfigRequest,
) (*api.GetOriginAccessControlConfigResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.Id)
	if id == "" {
		return nil, noSuchOriginAccessControl("")
	}

	var rec *oacRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[oacRecord](tx, key(m.AccountID, kindOAC, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchOriginAccessControl(id)
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	var cfg *api.OriginAccessControlConfig
	if rec.OAC != nil {
		cfg = rec.OAC.OriginAccessControlConfig
	}

	return &api.GetOriginAccessControlConfigResult{
		OriginAccessControlConfig: cfg,
		ETag:                      &rec.ETag,
	}, nil
}

func (s *Service) UpdateOriginAccessControl(
	ctx context.Context, in *api.UpdateOriginAccessControlRequest,
) (*api.UpdateOriginAccessControlResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.Id)
	if id == "" {
		return nil, noSuchOriginAccessControl("")
	}
	if in.OriginAccessControlConfig == nil {
		return nil, invalidArgument("OriginAccessControlConfig is required")
	}

	newName := common.Value(in.OriginAccessControlConfig.Name)
	if newName == "" {
		return nil, invalidArgument("Name is required")
	}

	var updated *oacRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[oacRecord](tx, key(m.AccountID, kindOAC, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchOriginAccessControl(id)
		}

		if in.IfMatch == nil || (*in.IfMatch != "*" && *in.IfMatch != rec.ETag) {
			return invalidIfMatchVersion()
		}

		oldName := ""
		if rec.OAC != nil && rec.OAC.OriginAccessControlConfig != nil {
			oldName = common.Value(rec.OAC.OriginAccessControlConfig.Name)
		}

		if oldName != newName {
			existingID, exists := tx.Get(key(m.AccountID, kindOACName, newName))
			if exists && string(existingID) != id {
				return originAccessControlAlreadyExists()
			}
			if oldName != "" {
				tx.Delete(key(m.AccountID, kindOACName, oldName))
			}
			tx.Put(key(m.AccountID, kindOACName, newName), []byte(id))
		}

		rec.ETag = newID("E")
		rec.OAC.OriginAccessControlConfig = in.OriginAccessControlConfig

		if writeErr := storage.Write(tx, key(m.AccountID, kindOAC, id), rec); writeErr != nil {
			return writeErr
		}
		updated = rec
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateOriginAccessControlResult{
		OriginAccessControl: updated.OAC,
		ETag:                &updated.ETag,
	}, nil
}

func (s *Service) DeleteOriginAccessControl(
	ctx context.Context, in *api.DeleteOriginAccessControlRequest,
) (*api.DeleteOriginAccessControlOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.Id)
	if id == "" {
		return nil, noSuchOriginAccessControl("")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[oacRecord](tx, key(m.AccountID, kindOAC, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchOriginAccessControl(id)
		}

		if in.IfMatch == nil || (*in.IfMatch != "*" && *in.IfMatch != rec.ETag) {
			return invalidIfMatchVersion()
		}

		// Check if in use by any distribution
		prefix := m.AccountID + "/" + service + "/" + kindDistribution + "/"
		for _, k := range tx.Keys(prefix) {
			parts := strings.Split(k, "/")
			if len(parts) != 4 {
				continue
			}
			dRec, dFound, dErr := storage.Read[distributionRecord](tx, k)
			if dErr != nil {
				return dErr
			}
			if dFound && dRec.Distribution != nil && dRec.Distribution.DistributionConfig != nil &&
				dRec.Distribution.DistributionConfig.Origins != nil {
				for _, orig := range dRec.Distribution.DistributionConfig.Origins.Items {
					if common.Value(orig.OriginAccessControlId) == id {
						return originAccessControlInUse()
					}
				}
			}
		}

		tx.Delete(key(m.AccountID, kindOAC, id))
		if rec.OAC != nil && rec.OAC.OriginAccessControlConfig != nil {
			tx.Delete(key(m.AccountID, kindOACName, common.Value(rec.OAC.OriginAccessControlConfig.Name)))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteOriginAccessControlOutput{}, nil
}

func (s *Service) ListOriginAccessControls(
	ctx context.Context, in *api.ListOriginAccessControlsRequest,
) (*api.ListOriginAccessControlsResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var items []api.OriginAccessControlSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := m.AccountID + "/" + service + "/" + kindOAC + "/"
		keys := tx.Keys(prefix)
		slices.Sort(keys)

		for _, k := range keys {
			rec, found, readErr := storage.Read[oacRecord](tx, k)
			if readErr != nil {
				return readErr
			}
			if found {
				items = append(items, toOACSummary(rec))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	maxItems := int64(100)
	if in != nil && in.MaxItems != nil && *in.MaxItems > 0 {
		maxItems = *in.MaxItems
	}

	marker := ""
	if in != nil && in.Marker != nil {
		marker = *in.Marker
	}

	start := 0
	if marker != "" {
		for i := range items {
			if common.Value(items[i].Id) > marker {
				start = i
				break
			}
			if i == len(items)-1 {
				start = len(items)
			}
		}
	}

	page := items[start:]
	truncated := false
	var nextMarker *string
	if int64(len(page)) > maxItems {
		truncated = true
		page = page[:maxItems]
		nextMarker = page[len(page)-1].Id
	}

	quantity := int64(len(page))
	return &api.ListOriginAccessControlsResult{
		OriginAccessControlList: &api.OriginAccessControlList{
			IsTruncated: &truncated,
			Items:       page,
			Marker:      &marker,
			MaxItems:    &maxItems,
			NextMarker:  nextMarker,
			Quantity:    &quantity,
		},
	}, nil
}
