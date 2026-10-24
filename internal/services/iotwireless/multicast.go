package iotwireless

import (
	"context"
	"slices"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusPending    = "Pending"
	statusSessionSet = "Session_Set"
	statusInitial    = "Initial"
)

// addMember adds an ID to a sorted list of IDs, which keeps lists in key order for paging.
func addMember(ids []string, id string) []string {
	i, found := slices.BinarySearch(ids, id)
	if found {
		return ids
	}

	return slices.Insert(ids, i, id)
}

// removeMember removes an ID from a list and reports whether it was there.
func removeMember(ids []string, id string) ([]string, bool) {
	i, found := slices.BinarySearch(ids, id)
	if !found {
		return ids, false
	}

	return slices.Delete(ids, i, i+1), true
}

// checkGateways fails unless every wireless gateway ID names a gateway that exists.
func checkGateways(tx storage.ReadTx, scope request.Scope, ids []string) error {
	for _, id := range ids {
		if _, err := load(tx, scope, kindGateway, id); err != nil {
			return err
		}
	}

	return nil
}

// multicastGateways are the gateway IDs a multicast group's participating gateways name.
func multicastGateways(p *api.ParticipatingGatewaysMulticast) []string {
	if p == nil {
		return nil
	}

	return p.GatewayList
}

// CreateMulticastGroup creates a multicast group; its participating gateways must exist.
func (s *Service) CreateMulticastGroup(
	ctx context.Context,
	in *api.CreateMulticastGroupRequest,
) (*api.CreateMulticastGroupResponse, error) {
	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		existing, hash, err := replay(tx, scope, kindMulticast, in.ClientRequestToken, in)
		if err != nil || existing != nil {
			r = existing
			return err
		}

		if checkErr := checkGateways(tx, scope, multicastGateways(in.LoRaWAN.ParticipatingGateways)); checkErr != nil {
			return checkErr
		}

		r = newRecord(scope, kindMulticast, uuid.NewV4().String(), tags)
		r.Name = common.Value(in.Name)
		r.Multicast = &api.GetMulticastGroupResponse{
			Arn: new(r.Arn), Id: new(r.ID), Name: in.Name, Description: in.Description, Status: new(statusPending),
			CreatedAt: new(common.Unix(time.Now())),
			LoRaWAN: &api.LoRaWANMulticastGet{
				RfRegion: in.LoRaWAN.RfRegion, DlClass: in.LoRaWAN.DlClass,
				ParticipatingGateways:    in.LoRaWAN.ParticipatingGateways,
				DefaultSessionParameters: in.LoRaWAN.DefaultSessionParameters,
			},
		}
		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return remember(tx, scope, in.ClientRequestToken, hash, r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.CreateMulticastGroupResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// GetMulticastGroup returns a multicast group; every device associated with it counts as requested.
func (s *Service) GetMulticastGroup(
	ctx context.Context,
	in *api.GetMulticastGroupRequest,
) (*api.GetMulticastGroupResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindMulticast, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	r.Multicast.LoRaWAN.NumberOfDevicesInGroup = new(int64(len(r.Devices)))
	r.Multicast.LoRaWAN.NumberOfDevicesRequested = new(int64(len(r.Devices)))
	return r.Multicast, nil
}

// ListMulticastGroups lists multicast groups.
func (s *Service) ListMulticastGroups(
	ctx context.Context,
	in *api.ListMulticastGroupsRequest,
) (*api.ListMulticastGroupsResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindMulticast))
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

	out := &api.ListMulticastGroupsResponse{NextToken: next, MulticastGroupList: []api.MulticastGroup{}}
	for _, r := range page {
		out.MulticastGroupList = append(out.MulticastGroupList, api.MulticastGroup{
			Arn: r.Multicast.Arn, Id: r.Multicast.Id, Name: r.Multicast.Name,
		})
	}

	return out, nil
}

// UpdateMulticastGroup changes a multicast group's name, description and LoRaWAN members.
func (s *Service) UpdateMulticastGroup(
	ctx context.Context,
	in *api.UpdateMulticastGroupRequest,
) (*api.UpdateMulticastGroupResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindMulticast, *in.Id)
		if err != nil {
			return err
		}

		g := r.Multicast
		if lw := in.LoRaWAN; lw != nil {
			if checkErr := checkGateways(tx, scope, multicastGateways(lw.ParticipatingGateways)); checkErr != nil {
				return checkErr
			}

			g.LoRaWAN.RfRegion = common.OrElse(lw.RfRegion, g.LoRaWAN.RfRegion)
			g.LoRaWAN.DlClass = common.OrElse(lw.DlClass, g.LoRaWAN.DlClass)
			g.LoRaWAN.ParticipatingGateways = common.OrElse(lw.ParticipatingGateways, g.LoRaWAN.ParticipatingGateways)
			g.LoRaWAN.DefaultSessionParameters = common.OrElse(
				lw.DefaultSessionParameters, g.LoRaWAN.DefaultSessionParameters,
			)
		}

		g.Name = common.OrElse(in.Name, g.Name)
		g.Description = common.OrElse(in.Description, g.Description)
		r.Name = common.Value(g.Name)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.UpdateMulticastGroupResponse{}, err
}

// DeleteMulticastGroup deletes a multicast group, its queued messages and its place in FUOTA tasks.
func (s *Service) DeleteMulticastGroup(
	ctx context.Context,
	in *api.DeleteMulticastGroupRequest,
) (*api.DeleteMulticastGroupResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindMulticast, *in.Id)
		if err != nil {
			return err
		}

		tasks, err := storage.Records[record](tx, scope.Prefix(serviceName, kindFuota))
		if err != nil {
			return err
		}

		for _, task := range tasks {
			var found bool
			if task.Groups, found = removeMember(task.Groups, r.ID); found {
				if checkErr := storage.Write(tx, scope.Key(serviceName, task.Kind, task.ID), task); checkErr != nil {
					return checkErr
				}
			}
		}

		for _, key := range tx.Keys(scope.Key(serviceName, kindGroupMessage, r.ID+"/")) {
			tx.Delete(key)
		}

		tx.Delete(indexKey(scope, kindMulticast, r.ID))
		tx.Delete(scope.Key(serviceName, kindMulticast, r.ID))
		return nil
	})

	return &api.DeleteMulticastGroupResponse{}, err
}

// AssociateWirelessDeviceWithMulticastGroup adds an existing LoRaWAN device to a multicast group.
func (s *Service) AssociateWirelessDeviceWithMulticastGroup(
	ctx context.Context,
	in *api.AssociateWirelessDeviceWithMulticastGroupRequest,
) (*api.AssociateWirelessDeviceWithMulticastGroupResponse, error) {
	err := s.member(ctx, kindMulticast, *in.Id, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if _, err := load(tx, scope, kindDevice, *in.WirelessDeviceId); err != nil {
			return err
		}

		r.Devices = addMember(r.Devices, *in.WirelessDeviceId)
		return nil
	})

	return &api.AssociateWirelessDeviceWithMulticastGroupResponse{}, err
}

// DisassociateWirelessDeviceFromMulticastGroup removes a device from a multicast group.
func (s *Service) DisassociateWirelessDeviceFromMulticastGroup(
	ctx context.Context,
	in *api.DisassociateWirelessDeviceFromMulticastGroupRequest,
) (*api.DisassociateWirelessDeviceFromMulticastGroupResponse, error) {
	err := s.member(ctx, kindMulticast, *in.Id, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		var found bool
		if r.Devices, found = removeMember(r.Devices, *in.WirelessDeviceId); !found {
			return missing("Association of the wireless device "+*in.WirelessDeviceId+" with the "+kindMulticast, r.ID)
		}

		return nil
	})

	return &api.DisassociateWirelessDeviceFromMulticastGroupResponse{}, err
}

// member changes the associations of a multicast group or FUOTA task.
func (s *Service) member(
	ctx context.Context,
	kind, id string,
	change func(storage.WriteTx, request.Scope, *record) error,
) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kind, id)
		if err != nil {
			return err
		}

		if err := change(tx, scope, r); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
}

// StartMulticastGroupSession records a session for a group. The session starts, and the group becomes Session_Set,
// when its start time arrives (now, if it has none) and ends after its timeout; nothing is transmitted.
func (s *Service) StartMulticastGroupSession(
	ctx context.Context,
	in *api.StartMulticastGroupSessionRequest,
) (*api.StartMulticastGroupSessionResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindMulticast, *in.Id)
		if err != nil {
			return err
		}

		if r.Session != nil {
			return conflict("The multicast group %s already has a session", r.ID)
		}

		session := *in.LoRaWAN
		session.SessionStartTime = common.OrElse(session.SessionStartTime, new(time.Now().UTC()))
		r.Session = &session
		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return storage.Write(
			tx,
			indexKey(scope, kindMulticast, r.ID),
			workItem{Scope: scope, Kind: kindMulticast, ID: r.ID},
		)
	})

	return &api.StartMulticastGroupSessionResponse{}, err
}

// GetMulticastGroupSession returns a group's session, or an empty one if it has none.
func (s *Service) GetMulticastGroupSession(
	ctx context.Context,
	in *api.GetMulticastGroupSessionRequest,
) (*api.GetMulticastGroupSessionResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindMulticast, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetMulticastGroupSessionResponse{LoRaWAN: common.OrElse(r.Session, &api.LoRaWANMulticastSession{})}, nil
}

// CancelMulticastGroupSession ends a group's session.
func (s *Service) CancelMulticastGroupSession(
	ctx context.Context,
	in *api.CancelMulticastGroupSessionRequest,
) (*api.CancelMulticastGroupSessionResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindMulticast, *in.Id)
		if err != nil {
			return err
		}

		if r.Session == nil {
			return missing("Session of the "+kindMulticast, r.ID)
		}

		r.Session, r.Multicast.Status = nil, new(statusPending)
		tx.Delete(indexKey(scope, kindMulticast, r.ID))
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.CancelMulticastGroupSessionResponse{}, err
}
