package iotwireless

import (
	"context"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusWaiting   = "FuotaSession_Waiting"
	statusInSession = "In_FuotaSession"
	statusDone      = "FuotaDone"
	deviceSuccess   = "Successful"
)

// fuotaDeviceStatus is the FUOTA status of every device of a task: Successful once the task is done.
func fuotaDeviceStatus(task *record) string {
	if common.Value(task.Fuota.Status) == statusDone {
		return deviceSuccess
	}

	return statusInitial
}

// checkImage fails unless a firmware image is an object in an S3 bucket of TinyStack.
func (s *Service) checkImage(ctx context.Context, image string) error {
	location, ok := strings.CutPrefix(image, "s3://")
	bucket, key, found := strings.Cut(location, "/")
	if !ok || !found || bucket == "" || key == "" {
		return invalid("The firmware update image %s is not an S3 URI", image)
	}

	if _, err := s.caller.Call(ctx, "s3", "HeadObject", map[string]any{"Bucket": bucket, "Key": key}); err != nil {
		return missing("Firmware update image", image)
	}

	return nil
}

// CreateFuotaTask creates a FUOTA task whose firmware image is an S3 object and whose role exists.
func (s *Service) CreateFuotaTask(
	ctx context.Context,
	in *api.CreateFuotaTaskRequest,
) (*api.CreateFuotaTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	if err := s.checkImage(ctx, *in.FirmwareUpdateImage); err != nil {
		return nil, err
	}

	if err := s.checkRole(ctx, scope, *in.FirmwareUpdateRole); err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		existing, hash, err := replay(tx, scope, kindFuota, in.ClientRequestToken, in)
		if err != nil || existing != nil {
			r = existing
			return err
		}

		r = newRecord(scope, kindFuota, uuid.NewV4().String(), tags)
		r.Name = common.Value(in.Name)
		r.Fuota = &api.GetFuotaTaskResponse{
			Arn:                 new(r.Arn),
			Id:                  new(r.ID),
			Name:                in.Name,
			Description:         in.Description,
			Status:              new(statusPending),
			CreatedAt:           new(common.Unix(time.Now())),
			FirmwareUpdateImage: in.FirmwareUpdateImage,
			FirmwareUpdateRole:  in.FirmwareUpdateRole,
			RedundancyPercent:   in.RedundancyPercent,
			FragmentSizeBytes:   in.FragmentSizeBytes,
			FragmentIntervalMS:  in.FragmentIntervalMS,
			Descriptor:          in.Descriptor,
		}
		if in.LoRaWAN != nil {
			r.Fuota.LoRaWAN = &api.LoRaWANFuotaTaskGetInfo{RfRegion: in.LoRaWAN.RfRegion}
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return remember(tx, scope, in.ClientRequestToken, hash, r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.CreateFuotaTaskResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// GetFuotaTask returns a FUOTA task.
func (s *Service) GetFuotaTask(ctx context.Context, in *api.GetFuotaTaskRequest) (*api.GetFuotaTaskResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindFuota, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.Fuota, nil
}

// ListFuotaTasks lists FUOTA tasks.
func (s *Service) ListFuotaTasks(
	ctx context.Context,
	in *api.ListFuotaTasksRequest,
) (*api.ListFuotaTasksResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindFuota))
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

	out := &api.ListFuotaTasksResponse{NextToken: next, FuotaTaskList: []api.FuotaTask{}}
	for _, r := range page {
		out.FuotaTaskList = append(
			out.FuotaTaskList,
			api.FuotaTask{Arn: r.Fuota.Arn, Id: r.Fuota.Id, Name: r.Fuota.Name},
		)
	}

	return out, nil
}

// pendingTask loads a FUOTA task and fails unless it has not started, which is the only time it can change.
func pendingTask(tx storage.ReadTx, scope request.Scope, id string) (*record, error) {
	r, err := load(tx, scope, kindFuota, id)
	if err != nil {
		return nil, err
	}

	if status := common.Value(r.Fuota.Status); status != statusPending {
		return nil, conflict("The FUOTA task %s is %s and can no longer change", id, status)
	}

	return r, nil
}

// UpdateFuotaTask changes a FUOTA task that has not started.
func (s *Service) UpdateFuotaTask(
	ctx context.Context,
	in *api.UpdateFuotaTaskRequest,
) (*api.UpdateFuotaTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.FirmwareUpdateImage != nil {
		if err := s.checkImage(ctx, *in.FirmwareUpdateImage); err != nil {
			return nil, err
		}
	}

	if in.FirmwareUpdateRole != nil {
		if err := s.checkRole(ctx, scope, *in.FirmwareUpdateRole); err != nil {
			return nil, err
		}
	}

	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := pendingTask(tx, scope, *in.Id)
		if err != nil {
			return err
		}

		f := r.Fuota
		f.Name = common.OrElse(in.Name, f.Name)
		f.Description = common.OrElse(in.Description, f.Description)
		f.FirmwareUpdateImage = common.OrElse(in.FirmwareUpdateImage, f.FirmwareUpdateImage)
		f.FirmwareUpdateRole = common.OrElse(in.FirmwareUpdateRole, f.FirmwareUpdateRole)
		f.RedundancyPercent = common.OrElse(in.RedundancyPercent, f.RedundancyPercent)
		f.FragmentSizeBytes = common.OrElse(in.FragmentSizeBytes, f.FragmentSizeBytes)
		f.FragmentIntervalMS = common.OrElse(in.FragmentIntervalMS, f.FragmentIntervalMS)
		f.Descriptor = common.OrElse(in.Descriptor, f.Descriptor)
		if in.LoRaWAN != nil {
			if f.LoRaWAN == nil {
				f.LoRaWAN = &api.LoRaWANFuotaTaskGetInfo{}
			}

			f.LoRaWAN.RfRegion = common.OrElse(in.LoRaWAN.RfRegion, f.LoRaWAN.RfRegion)
		}

		r.Name = common.Value(f.Name)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.UpdateFuotaTaskResponse{}, updateErr
}

// DeleteFuotaTask deletes a FUOTA task.
func (s *Service) DeleteFuotaTask(
	ctx context.Context,
	in *api.DeleteFuotaTaskRequest,
) (*api.DeleteFuotaTaskResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := load(tx, scope, kindFuota, *in.Id); err != nil {
			return err
		}

		tx.Delete(indexKey(scope, kindFuota, *in.Id))
		tx.Delete(scope.Key(serviceName, kindResourceLog, kindFuota+"/"+*in.Id))
		tx.Delete(scope.Key(serviceName, kindFuota, *in.Id))
		return nil
	})

	return &api.DeleteFuotaTaskResponse{}, err
}

// changeTask changes the associations of a FUOTA task that has not started.
func (s *Service) changeTask(
	ctx context.Context,
	id string,
	change func(storage.WriteTx, request.Scope, *record) error,
) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := pendingTask(tx, scope, id)
		if err != nil {
			return err
		}

		if err := change(tx, scope, r); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
}

// AssociateWirelessDeviceWithFuotaTask adds an existing device to a FUOTA task.
func (s *Service) AssociateWirelessDeviceWithFuotaTask(
	ctx context.Context,
	in *api.AssociateWirelessDeviceWithFuotaTaskRequest,
) (*api.AssociateWirelessDeviceWithFuotaTaskResponse, error) {
	err := s.changeTask(ctx, *in.Id, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if _, err := load(tx, scope, kindDevice, *in.WirelessDeviceId); err != nil {
			return err
		}

		r.Devices = addMember(r.Devices, *in.WirelessDeviceId)
		return nil
	})

	return &api.AssociateWirelessDeviceWithFuotaTaskResponse{}, err
}

// DisassociateWirelessDeviceFromFuotaTask removes a device from a FUOTA task.
func (s *Service) DisassociateWirelessDeviceFromFuotaTask(
	ctx context.Context,
	in *api.DisassociateWirelessDeviceFromFuotaTaskRequest,
) (*api.DisassociateWirelessDeviceFromFuotaTaskResponse, error) {
	err := s.changeTask(ctx, *in.Id, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		var found bool
		if r.Devices, found = removeMember(r.Devices, *in.WirelessDeviceId); !found {
			return missing("Association of the wireless device "+*in.WirelessDeviceId+" with the "+kindFuota, r.ID)
		}

		return nil
	})

	return &api.DisassociateWirelessDeviceFromFuotaTaskResponse{}, err
}

// AssociateMulticastGroupWithFuotaTask adds an existing multicast group to a FUOTA task.
func (s *Service) AssociateMulticastGroupWithFuotaTask(
	ctx context.Context,
	in *api.AssociateMulticastGroupWithFuotaTaskRequest,
) (*api.AssociateMulticastGroupWithFuotaTaskResponse, error) {
	err := s.changeTask(ctx, *in.Id, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if _, err := load(tx, scope, kindMulticast, *in.MulticastGroupId); err != nil {
			return err
		}

		r.Groups = addMember(r.Groups, *in.MulticastGroupId)
		return nil
	})

	return &api.AssociateMulticastGroupWithFuotaTaskResponse{}, err
}

// DisassociateMulticastGroupFromFuotaTask removes a multicast group from a FUOTA task.
func (s *Service) DisassociateMulticastGroupFromFuotaTask(
	ctx context.Context,
	in *api.DisassociateMulticastGroupFromFuotaTaskRequest,
) (*api.DisassociateMulticastGroupFromFuotaTaskResponse, error) {
	err := s.changeTask(ctx, *in.Id, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		var found bool
		if r.Groups, found = removeMember(r.Groups, *in.MulticastGroupId); !found {
			return missing("Association of the multicast group "+*in.MulticastGroupId+" with the "+kindFuota, r.ID)
		}

		return nil
	})

	return &api.DisassociateMulticastGroupFromFuotaTaskResponse{}, err
}

// ListMulticastGroupsByFuotaTask lists the multicast groups associated with a FUOTA task.
func (s *Service) ListMulticastGroupsByFuotaTask(
	ctx context.Context,
	in *api.ListMulticastGroupsByFuotaTaskRequest,
) (*api.ListMulticastGroupsByFuotaTaskResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindFuota, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		r.Groups,
		func(id string) string { return id },
		in.NextToken,
		pageSize(in.MaxResults),
		badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListMulticastGroupsByFuotaTaskResponse{
		NextToken:          next,
		MulticastGroupList: []api.MulticastGroupByFuotaTask{},
	}
	for _, id := range page {
		out.MulticastGroupList = append(out.MulticastGroupList, api.MulticastGroupByFuotaTask{Id: new(id)})
	}

	return out, nil
}

// StartFuotaTask starts a pending task: it waits for its start time (now, if it has none), then runs its session and
// finishes. Nothing is transmitted.
func (s *Service) StartFuotaTask(
	ctx context.Context,
	in *api.StartFuotaTaskRequest,
) (*api.StartFuotaTaskResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := pendingTask(tx, scope, *in.Id)
		if err != nil {
			return err
		}

		start := time.Now().UTC()
		if in.LoRaWAN != nil && in.LoRaWAN.StartTime != nil {
			start = *in.LoRaWAN.StartTime
		}

		if r.Fuota.LoRaWAN == nil {
			r.Fuota.LoRaWAN = &api.LoRaWANFuotaTaskGetInfo{}
		}

		r.Fuota.LoRaWAN.StartTime, r.Fuota.Status = new(start), new(statusWaiting)
		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, indexKey(scope, kindFuota, r.ID), workItem{Scope: scope, Kind: kindFuota, ID: r.ID})
	})

	return &api.StartFuotaTaskResponse{}, err
}
