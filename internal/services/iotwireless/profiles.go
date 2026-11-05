package iotwireless

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// createProfile makes a profile record, or replays the one a ClientRequestToken made, and returns its ID and ARN.
func (s *Service) createProfile(
	ctx context.Context,
	kind string,
	token *string,
	in any,
	tags []api.Tag,
	fill func(r *record),
) (*record, error) {
	made, err := tagsOf(tags)
	if err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := idempotency.Key(scope, serviceName, kind, token)
		existing, found, err := idempotency.Replay[*record](
			tx,
			key,
			in,
			conflict("The ClientRequestToken %s was used with different parameters", common.Value(token)),
		)
		if err != nil || found {
			r = existing
			return err
		}

		r = newRecord(scope, kind, uuid.NewV4().String(), made)
		fill(r)
		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return idempotency.Remember(tx, key, in, r)
	})

	return r, updateErr
}

// CreateDeviceProfile creates a LoRaWAN or Sidewalk device profile; a Sidewalk profile gets the application server
// public key AWS would assign, and is not qualified.
func (s *Service) CreateDeviceProfile(
	ctx context.Context,
	in *api.CreateDeviceProfileRequest,
) (*api.CreateDeviceProfileResponse, error) {
	if (in.LoRaWAN == nil) == (in.Sidewalk == nil) {
		return nil, invalid("Exactly one of LoRaWAN and Sidewalk is required")
	}

	r, err := s.createProfile(ctx, kindDeviceProfile, in.ClientRequestToken, in, in.Tags, func(r *record) {
		r.Name = common.Value(in.Name)
		r.DeviceProfile = &api.GetDeviceProfileResponse{
			Arn: new(r.Arn), Id: new(r.ID), Name: in.Name, LoRaWAN: in.LoRaWAN,
		}

		if in.Sidewalk != nil {
			key := make([]byte, 32)
			_, _ = rand.Read(key)
			r.DeviceProfile.Sidewalk = &api.SidewalkGetDeviceProfile{
				ApplicationServerPublicKey: new(hex.EncodeToString(key)), QualificationStatus: new(false),
			}
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDeviceProfileResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// GetDeviceProfile returns a device profile.
func (s *Service) GetDeviceProfile(
	ctx context.Context,
	in *api.GetDeviceProfileRequest,
) (*api.GetDeviceProfileResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindDeviceProfile, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.DeviceProfile, nil
}

// ListDeviceProfiles lists device profiles, of one type if asked.
func (s *Service) ListDeviceProfiles(
	ctx context.Context,
	in *api.ListDeviceProfilesRequest,
) (*api.ListDeviceProfilesResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindDeviceProfile))
		return err
	})
	if err != nil {
		return nil, err
	}

	var matching []*record
	for _, r := range all {
		switch common.Value(in.DeviceProfileType) {
		case "Sidewalk":
			if r.DeviceProfile.Sidewalk == nil {
				continue
			}
		case typeLoRaWAN:
			if r.DeviceProfile.LoRaWAN == nil {
				continue
			}
		}

		matching = append(matching, r)
	}

	page, next, err := common.Page(
		matching, func(r *record) string { return r.ID }, in.NextToken, pageSize(in.MaxResults), badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListDeviceProfilesResponse{NextToken: next, DeviceProfileList: []api.DeviceProfile{}}
	for _, r := range page {
		out.DeviceProfileList = append(out.DeviceProfileList, api.DeviceProfile{
			Arn: r.DeviceProfile.Arn, Id: r.DeviceProfile.Id, Name: r.DeviceProfile.Name,
		})
	}

	return out, nil
}

// DeleteDeviceProfile deletes a device profile that no wireless device uses.
func (s *Service) DeleteDeviceProfile(
	ctx context.Context,
	in *api.DeleteDeviceProfileRequest,
) (*api.DeleteDeviceProfileResponse, error) {
	err := s.deleteProfile(
		ctx,
		kindDeviceProfile,
		*in.Id,
		func(d *api.LoRaWANDevice) *string { return d.DeviceProfileId },
	)
	return &api.DeleteDeviceProfileResponse{}, err
}

// deleteProfile deletes a profile unless a wireless device references it with ref.
func (s *Service) deleteProfile(ctx context.Context, kind, id string, ref func(*api.LoRaWANDevice) *string) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := load(tx, scope, kind, id); err != nil {
			return err
		}

		devices, err := storage.Records[record](tx, scope.Prefix(serviceName, kindDevice))
		if err != nil {
			return err
		}

		for _, d := range devices {
			if d.Device.LoRaWAN != nil && common.Value(ref(d.Device.LoRaWAN)) == id {
				return conflict("The %s %s is used by the wireless device %s", kind, id, d.ID)
			}
		}

		tx.Delete(scope.Key(serviceName, kind, id))
		return nil
	})
}

// CreateServiceProfile creates a service profile; the LoRaWAN fields it takes are stored as given.
func (s *Service) CreateServiceProfile(
	ctx context.Context,
	in *api.CreateServiceProfileRequest,
) (*api.CreateServiceProfileResponse, error) {
	r, err := s.createProfile(ctx, kindServiceProfile, in.ClientRequestToken, in, in.Tags, func(r *record) {
		r.Name = common.Value(in.Name)
		r.ServiceProfile = &api.GetServiceProfileResponse{Arn: new(r.Arn), Id: new(r.ID), Name: in.Name}
		if p := in.LoRaWAN; p != nil {
			r.ServiceProfile.LoRaWAN = &api.LoRaWANGetServiceProfileInfo{
				AddGwMetadata: p.AddGwMetadata, DrMin: p.DrMin, DrMax: p.DrMax, PrAllowed: p.PrAllowed,
				RaAllowed: p.RaAllowed, TxPowerIndexMin: p.TxPowerIndexMin, TxPowerIndexMax: p.TxPowerIndexMax,
				NbTransMin: p.NbTransMin, NbTransMax: p.NbTransMax,
			}
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateServiceProfileResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// GetServiceProfile returns a service profile.
func (s *Service) GetServiceProfile(
	ctx context.Context,
	in *api.GetServiceProfileRequest,
) (*api.GetServiceProfileResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindServiceProfile, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.ServiceProfile, nil
}

// ListServiceProfiles lists service profiles.
func (s *Service) ListServiceProfiles(
	ctx context.Context,
	in *api.ListServiceProfilesRequest,
) (*api.ListServiceProfilesResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindServiceProfile))
		return err
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		all,
		func(r *record) string { return r.ID },
		in.NextToken,
		pageSize(in.MaxResults),
		badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListServiceProfilesResponse{NextToken: next, ServiceProfileList: []api.ServiceProfile{}}
	for _, r := range page {
		out.ServiceProfileList = append(out.ServiceProfileList, api.ServiceProfile{
			Arn: r.ServiceProfile.Arn, Id: r.ServiceProfile.Id, Name: r.ServiceProfile.Name,
		})
	}

	return out, nil
}

// DeleteServiceProfile deletes a service profile that no wireless device uses.
func (s *Service) DeleteServiceProfile(
	ctx context.Context,
	in *api.DeleteServiceProfileRequest,
) (*api.DeleteServiceProfileResponse, error) {
	err := s.deleteProfile(
		ctx,
		kindServiceProfile,
		*in.Id,
		func(d *api.LoRaWANDevice) *string { return d.ServiceProfileId },
	)
	return &api.DeleteServiceProfileResponse{}, err
}
