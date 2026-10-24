package iotwireless

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	idWirelessDevice = "WirelessDeviceId"
	idDevEui         = "DevEui"
)

// normalizeEUI is an EUI without separators and in lower case, which is how the indexes compare EUIs.
func normalizeEUI(eui string) string {
	var out strings.Builder
	for _, c := range eui {
		if c != '-' && c != ':' && !unicode.IsSpace(c) {
			out.WriteRune(unicode.ToLower(c))
		}
	}

	return out.String()
}

// activations counts the activation modes a LoRaWAN device sets.
func activations(lw *api.LoRaWANDevice) int {
	n := 0
	for _, set := range []bool{lw.OtaaV1_1 != nil, lw.OtaaV1_0_x != nil, lw.AbpV1_1 != nil, lw.AbpV1_0_x != nil} {
		if set {
			n++
		}
	}

	return n
}

// checkApplications fails unless every application port routes to a destination that exists.
func checkApplications(tx storage.ReadTx, scope request.Scope, apps []api.ApplicationConfig) error {
	for _, app := range apps {
		if app.DestinationName == nil {
			continue
		}

		if _, err := load(tx, scope, kindDestination, *app.DestinationName); err != nil {
			return err
		}
	}

	return nil
}

// checkProfiles fails unless the device and service profiles a device names exist.
func checkProfiles(tx storage.ReadTx, scope request.Scope, deviceProfile, serviceProfile *string) error {
	if deviceProfile != nil {
		if _, err := load(tx, scope, kindDeviceProfile, *deviceProfile); err != nil {
			return err
		}
	}

	if serviceProfile != nil {
		if _, err := load(tx, scope, kindServiceProfile, *serviceProfile); err != nil {
			return err
		}
	}

	return nil
}

// CreateWirelessDevice creates a LoRaWAN wireless device; Sidewalk devices need Sidewalk account linking, which is
// not supported.
func (s *Service) CreateWirelessDevice(
	ctx context.Context,
	in *api.CreateWirelessDeviceRequest,
) (*api.CreateWirelessDeviceResponse, error) {
	lw := in.LoRaWAN
	switch {
	case *in.Type != typeLoRaWAN:
		return nil, invalid("Wireless devices of type %s are not supported", *in.Type)
	case lw == nil || lw.DevEui == nil:
		return nil, invalid("LoRaWAN.DevEui is required")
	case activations(lw) != 1:
		return nil, invalid("Exactly one of OtaaV1_1, OtaaV1_0_x, AbpV1_1 and AbpV1_0_x is required")
	}

	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		existing, hash, err := replay(tx, scope, kindDevice, in.ClientRequestToken, in)
		if err != nil || existing != nil {
			r = existing
			return err
		}

		if _, err = load(tx, scope, kindDestination, *in.DestinationName); err != nil {
			return err
		}

		if err := checkProfiles(tx, scope, lw.DeviceProfileId, lw.ServiceProfileId); err != nil {
			return err
		}

		if lw.FPorts != nil {
			if err := checkApplications(tx, scope, lw.FPorts.Applications); err != nil {
				return err
			}
		}

		r = newRecord(scope, kindDevice, uuid.NewV4().String(), tags)
		if checkErr := claim(tx, scope, kindDevEui, normalizeEUI(*lw.DevEui), r.ID); checkErr != nil {
			return checkErr
		}

		r.Name = common.Value(in.Name)
		r.Device = &api.GetWirelessDeviceResponse{
			Arn: new(r.Arn), Id: new(r.ID), Type: in.Type, Name: in.Name, Description: in.Description,
			DestinationName: in.DestinationName, LoRaWAN: lw, Positioning: in.Positioning,
		}
		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return remember(tx, scope, in.ClientRequestToken, hash, r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.CreateWirelessDeviceResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// deviceID resolves a device identifier of one of the types the API takes.
func deviceID(tx storage.ReadTx, scope request.Scope, identifier, idType string) (string, error) {
	var id string
	var found bool
	switch idType {
	case idWirelessDevice:
		return identifier, nil
	case idDevEui:
		id, found = indexOf(tx, scope, kindDevEui, normalizeEUI(identifier))
	case idThingName:
		var kind string
		kind, id, found = thingOwner(tx, scope, identifier)
		found = found && kind == kindDevice
	}

	if !found {
		return "", missing(kindDevice, identifier)
	}

	return id, nil
}

// GetWirelessDevice returns a wireless device by ID, DevEui or thing name.
func (s *Service) GetWirelessDevice(
	ctx context.Context,
	in *api.GetWirelessDeviceRequest,
) (*api.GetWirelessDeviceResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		id, err := deviceID(tx, scope, *in.Identifier, *in.IdentifierType)
		if err != nil {
			return err
		}

		r, err = load(tx, scope, kindDevice, id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.Device, nil
}

// ListWirelessDevices lists wireless devices that match every filter given.
func (s *Service) ListWirelessDevices(
	ctx context.Context,
	in *api.ListWirelessDevicesRequest,
) (*api.ListWirelessDevicesResponse, error) {
	var all []*record
	var fuota, group *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		if in.FuotaTaskId != nil {
			if fuota, err = load(tx, scope, kindFuota, *in.FuotaTaskId); err != nil {
				return err
			}
		}

		if in.MulticastGroupId != nil {
			if group, err = load(tx, scope, kindMulticast, *in.MulticastGroupId); err != nil {
				return err
			}
		}

		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindDevice))
		return err
	})
	if err != nil {
		return nil, err
	}

	var matching []*record
	for _, r := range all {
		if listed(r, in) && (fuota == nil || slices.Contains(fuota.Devices, r.ID)) &&
			(group == nil || slices.Contains(group.Devices, r.ID)) {
			matching = append(matching, r)
		}
	}

	page, next, err := common.Page(
		matching, func(r *record) string { return r.ID }, in.NextToken, pageSize(in.MaxResults), badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListWirelessDevicesResponse{NextToken: next, WirelessDeviceList: []api.WirelessDeviceStatistics{}}
	for _, r := range page {
		d := r.Device
		item := api.WirelessDeviceStatistics{
			Arn: d.Arn, Id: d.Id, Type: d.Type, Name: d.Name, DestinationName: d.DestinationName,
			Positioning: d.Positioning, LoRaWAN: &api.LoRaWANListDevice{DevEui: d.LoRaWAN.DevEui},
		}
		if r.LastUplink != "" {
			item.LastUplinkReceivedAt = new(r.LastUplink)
		}

		if fuota != nil {
			item.FuotaDeviceStatus = new(fuotaDeviceStatus(fuota))
		}

		if group != nil {
			item.MulticastDeviceStatus = new(statusInitial)
		}

		out.WirelessDeviceList = append(out.WirelessDeviceList, item)
	}

	return out, nil
}

// listed reports whether a device passes the filters that name its own attributes.
func listed(r *record, in *api.ListWirelessDevicesRequest) bool {
	d := r.Device
	return (in.DestinationName == nil || *in.DestinationName == common.Value(d.DestinationName)) &&
		(in.WirelessDeviceType == nil || *in.WirelessDeviceType == common.Value(d.Type)) &&
		(in.DeviceProfileId == nil || *in.DeviceProfileId == common.Value(d.LoRaWAN.DeviceProfileId)) &&
		(in.ServiceProfileId == nil || *in.ServiceProfileId == common.Value(d.LoRaWAN.ServiceProfileId))
}

// UpdateWirelessDevice changes a device's destination, profiles, ports and descriptive members. Activation keys and
// the DevEui cannot change; a frame counter start can, for the ABP version the device uses.
func (s *Service) UpdateWirelessDevice(
	ctx context.Context,
	in *api.UpdateWirelessDeviceRequest,
) (*api.UpdateWirelessDeviceResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDevice, *in.Id)
		if err != nil {
			return err
		}

		d := r.Device
		if in.DestinationName != nil {
			if _, err = load(tx, scope, kindDestination, *in.DestinationName); err != nil {
				return err
			}

			d.DestinationName = in.DestinationName
		}

		if err := updateLoRaWAN(tx, scope, d.LoRaWAN, in.LoRaWAN); err != nil {
			return err
		}

		d.Name = common.OrElse(in.Name, d.Name)
		d.Description = common.OrElse(in.Description, d.Description)
		d.Positioning = common.OrElse(in.Positioning, d.Positioning)
		r.Name = common.Value(d.Name)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.UpdateWirelessDeviceResponse{}, err
}

// updateLoRaWAN applies the LoRaWAN members of a device update to the device.
func updateLoRaWAN(tx storage.ReadTx, scope request.Scope, lw *api.LoRaWANDevice, up *api.LoRaWANUpdateDevice) error {
	if up == nil {
		return nil
	}

	if err := checkProfiles(tx, scope, up.DeviceProfileId, up.ServiceProfileId); err != nil {
		return err
	}

	switch {
	case up.AbpV1_1 != nil && lw.AbpV1_1 == nil:
		return invalid("The wireless device does not use ABP v1.1")
	case up.AbpV1_0_x != nil && lw.AbpV1_0_x == nil:
		return invalid("The wireless device does not use ABP v1.0.x")
	}

	if up.FPorts != nil {
		if err := checkApplications(tx, scope, up.FPorts.Applications); err != nil {
			return err
		}

		if lw.FPorts == nil {
			lw.FPorts = &api.FPorts{}
		}

		lw.FPorts.Positioning = common.OrElse(up.FPorts.Positioning, lw.FPorts.Positioning)
		if up.FPorts.Applications != nil {
			lw.FPorts.Applications = up.FPorts.Applications
		}
	}

	lw.DeviceProfileId = common.OrElse(up.DeviceProfileId, lw.DeviceProfileId)
	lw.ServiceProfileId = common.OrElse(up.ServiceProfileId, lw.ServiceProfileId)
	if up.AbpV1_1 != nil {
		lw.AbpV1_1.FCntStart = common.OrElse(up.AbpV1_1.FCntStart, lw.AbpV1_1.FCntStart)
	}

	if up.AbpV1_0_x != nil {
		lw.AbpV1_0_x.FCntStart = common.OrElse(up.AbpV1_0_x.FCntStart, lw.AbpV1_0_x.FCntStart)
	}

	return nil
}

// DeleteWirelessDevice deletes a device with its associations and queued messages.
func (s *Service) DeleteWirelessDevice(
	ctx context.Context,
	in *api.DeleteWirelessDeviceRequest,
) (*api.DeleteWirelessDeviceResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDevice, *in.Id)
		if err != nil {
			return err
		}

		tx.Delete(scope.Key(serviceName, kindDevEui, normalizeEUI(*r.Device.LoRaWAN.DevEui)))
		if r.Device.ThingName != nil {
			tx.Delete(scope.Key(serviceName, kindThing, *r.Device.ThingName))
		}

		for _, key := range tx.Keys(scope.Key(serviceName, kindMessage, r.ID+"/")) {
			tx.Delete(key)
		}

		tx.Delete(scope.Key(serviceName, kindResourceLog, kindDevice+"/"+r.ID))
		tx.Delete(scope.Key(serviceName, kindDevice, r.ID))
		return leaveGroups(tx, scope, r.ID)
	})

	return &api.DeleteWirelessDeviceResponse{}, err
}

// leaveGroups removes a deleted device from the multicast groups and FUOTA tasks it was associated with.
func leaveGroups(tx storage.WriteTx, scope request.Scope, id string) error {
	for _, kind := range []string{kindMulticast, kindFuota} {
		all, err := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if err != nil {
			return err
		}

		for _, r := range all {
			if !slices.Contains(r.Devices, id) {
				continue
			}

			r.Devices = slices.DeleteFunc(r.Devices, func(d string) bool { return d == id })
			if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
				return checkErr
			}
		}
	}

	return nil
}

// AssociateWirelessDeviceWithThing associates a device with an existing IoT thing.
func (s *Service) AssociateWirelessDeviceWithThing(
	ctx context.Context,
	in *api.AssociateWirelessDeviceWithThingRequest,
) (*api.AssociateWirelessDeviceWithThingResponse, error) {
	return &api.AssociateWirelessDeviceWithThingResponse{}, s.associateThing(ctx, kindDevice, *in.Id, *in.ThingArn)
}

// DisassociateWirelessDeviceFromThing removes a device's association with its thing.
func (s *Service) DisassociateWirelessDeviceFromThing(
	ctx context.Context,
	in *api.DisassociateWirelessDeviceFromThingRequest,
) (*api.DisassociateWirelessDeviceFromThingResponse, error) {
	return &api.DisassociateWirelessDeviceFromThingResponse{}, s.disassociateThing(ctx, kindDevice, *in.Id)
}
