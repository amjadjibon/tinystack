package iotwireless

import (
	"context"
	"uuid"

	iotapi "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	idWirelessGateway = "WirelessGatewayId"
	idGatewayEui      = "GatewayEui"
	statusConnected   = "Connected"
	statusDisconnect  = "Disconnected"
)

// CreateWirelessGateway creates a LoRaWAN wireless gateway; its GatewayEui is unique in the account and Region.
func (s *Service) CreateWirelessGateway(
	ctx context.Context,
	in *api.CreateWirelessGatewayRequest,
) (*api.CreateWirelessGatewayResponse, error) {
	if in.LoRaWAN.GatewayEui == nil || in.LoRaWAN.RfRegion == nil {
		return nil, invalid("LoRaWAN.GatewayEui and LoRaWAN.RfRegion are required")
	}

	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := idempotency.Key(scope, serviceName, kindGateway, in.ClientRequestToken)
		existing, found, err := idempotency.Replay[*record](
			tx,
			key,
			in,
			conflict(
				"The ClientRequestToken %s was used with different parameters",
				common.Value(in.ClientRequestToken),
			),
		)
		if err != nil || found {
			r = existing
			return err
		}

		r = newRecord(scope, kindGateway, uuid.NewV4().String(), tags)
		if checkErr := claim(tx, scope, kindGatewayEui, normalizeEUI(*in.LoRaWAN.GatewayEui), r.ID); checkErr != nil {
			return checkErr
		}

		r.Name = common.Value(in.Name)
		r.Gateway = &api.GetWirelessGatewayResponse{
			Arn: new(r.Arn), Id: new(r.ID), Name: in.Name, Description: in.Description, LoRaWAN: in.LoRaWAN,
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return idempotency.Remember(tx, key, in, r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.CreateWirelessGatewayResponse{Arn: new(r.Arn), Id: new(r.ID)}, nil
}

// gatewayID resolves a gateway identifier of one of the types the API takes.
func gatewayID(tx storage.ReadTx, scope request.Scope, identifier, idType string) (string, error) {
	var id string
	var found bool
	switch idType {
	case idWirelessGateway:
		return identifier, nil
	case idGatewayEui:
		id, found = indexOf(tx, scope, kindGatewayEui, normalizeEUI(identifier))
	case idThingName:
		var kind string
		kind, id, found = thingOwner(tx, scope, identifier)
		found = found && kind == kindGateway
	}

	if !found {
		return "", missing(kindGateway, identifier)
	}

	return id, nil
}

// GetWirelessGateway returns a wireless gateway by ID, GatewayEui or thing name.
func (s *Service) GetWirelessGateway(
	ctx context.Context,
	in *api.GetWirelessGatewayRequest,
) (*api.GetWirelessGatewayResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		id, err := gatewayID(tx, scope, *in.Identifier, *in.IdentifierType)
		if err != nil {
			return err
		}

		r, err = load(tx, scope, kindGateway, id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.Gateway, nil
}

// ListWirelessGateways lists wireless gateways.
func (s *Service) ListWirelessGateways(
	ctx context.Context,
	in *api.ListWirelessGatewaysRequest,
) (*api.ListWirelessGatewaysResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindGateway))
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

	out := &api.ListWirelessGatewaysResponse{NextToken: next, WirelessGatewayList: []api.WirelessGatewayStatistics{}}
	for _, r := range page {
		g := r.Gateway
		item := api.WirelessGatewayStatistics{
			Arn: g.Arn, Id: g.Id, Name: g.Name, Description: g.Description, LoRaWAN: g.LoRaWAN,
		}

		if r.LastUplink != "" {
			item.LastUplinkReceivedAt = new(r.LastUplink)
		}

		out.WirelessGatewayList = append(out.WirelessGatewayList, item)
	}

	return out, nil
}

// UpdateWirelessGateway changes a gateway's name, description and LoRaWAN filters.
func (s *Service) UpdateWirelessGateway(
	ctx context.Context,
	in *api.UpdateWirelessGatewayRequest,
) (*api.UpdateWirelessGatewayResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindGateway, *in.Id)
		if err != nil {
			return err
		}

		g := r.Gateway
		g.Name = common.OrElse(in.Name, g.Name)
		g.Description = common.OrElse(in.Description, g.Description)
		g.LoRaWAN.MaxEirp = common.OrElse(in.MaxEirp, g.LoRaWAN.MaxEirp)
		if in.JoinEuiFilters != nil {
			g.LoRaWAN.JoinEuiFilters = in.JoinEuiFilters
		}

		if in.NetIdFilters != nil {
			g.LoRaWAN.NetIdFilters = in.NetIdFilters
		}

		r.Name = common.Value(g.Name)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.UpdateWirelessGatewayResponse{}, err
}

// DeleteWirelessGateway deletes a gateway with its associations.
func (s *Service) DeleteWirelessGateway(
	ctx context.Context,
	in *api.DeleteWirelessGatewayRequest,
) (*api.DeleteWirelessGatewayResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindGateway, *in.Id)
		if err != nil {
			return err
		}

		tx.Delete(scope.Key(serviceName, kindGatewayEui, normalizeEUI(*r.Gateway.LoRaWAN.GatewayEui)))
		if r.Gateway.ThingName != nil {
			tx.Delete(scope.Key(serviceName, kindThing, *r.Gateway.ThingName))
		}

		if r.Certificate != "" {
			tx.Delete(scope.Key(serviceName, kindCertificate, r.Certificate))
		}

		tx.Delete(scope.Key(serviceName, kindResourceLog, kindGateway+"/"+r.ID))
		tx.Delete(scope.Key(serviceName, kindGateway, r.ID))
		return nil
	})

	return &api.DeleteWirelessGatewayResponse{}, err
}

// AssociateWirelessGatewayWithThing associates a gateway with an existing IoT thing.
func (s *Service) AssociateWirelessGatewayWithThing(
	ctx context.Context,
	in *api.AssociateWirelessGatewayWithThingRequest,
) (*api.AssociateWirelessGatewayWithThingResponse, error) {
	return &api.AssociateWirelessGatewayWithThingResponse{}, s.associateThing(ctx, kindGateway, *in.Id, *in.ThingArn)
}

// DisassociateWirelessGatewayFromThing removes a gateway's association with its thing.
func (s *Service) DisassociateWirelessGatewayFromThing(
	ctx context.Context,
	in *api.DisassociateWirelessGatewayFromThingRequest,
) (*api.DisassociateWirelessGatewayFromThingResponse, error) {
	return &api.DisassociateWirelessGatewayFromThingResponse{}, s.disassociateThing(ctx, kindGateway, *in.Id)
}

// AssociateWirelessGatewayWithCertificate associates a gateway with an existing IoT certificate. A certificate can
// belong to one gateway.
func (s *Service) AssociateWirelessGatewayWithCertificate(
	ctx context.Context,
	in *api.AssociateWirelessGatewayWithCertificateRequest,
) (*api.AssociateWirelessGatewayWithCertificateResponse, error) {
	_, err := s.things.DescribeCertificate(ctx, &iotapi.DescribeCertificateRequest{CertificateId: in.IotCertificateId})
	if err != nil {
		return nil, err
	}

	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindGateway, *in.Id)
		if err != nil {
			return err
		}

		if r.Certificate != "" && r.Certificate != *in.IotCertificateId {
			return conflict(
				"The wireless gateway %s is already associated with the certificate %s",
				r.ID,
				r.Certificate,
			)
		}

		if checkErr := claim(tx, scope, kindCertificate, *in.IotCertificateId, r.ID); checkErr != nil {
			return checkErr
		}

		r.Certificate = *in.IotCertificateId
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.AssociateWirelessGatewayWithCertificateResponse{IotCertificateId: in.IotCertificateId}, nil
}

// DisassociateWirelessGatewayFromCertificate removes a gateway's association with its certificate.
func (s *Service) DisassociateWirelessGatewayFromCertificate(
	ctx context.Context,
	in *api.DisassociateWirelessGatewayFromCertificateRequest,
) (*api.DisassociateWirelessGatewayFromCertificateResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindGateway, *in.Id)
		if err != nil {
			return err
		}

		if r.Certificate == "" {
			return missing("Certificate association of the "+kindGateway, r.ID)
		}

		tx.Delete(scope.Key(serviceName, kindCertificate, r.Certificate))
		r.Certificate = ""
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.DisassociateWirelessGatewayFromCertificateResponse{}, err
}

// GetWirelessGatewayCertificate returns the certificate a gateway is associated with. TinyStack runs no LoRaWAN
// network server, so there is no network server certificate.
func (s *Service) GetWirelessGatewayCertificate(
	ctx context.Context,
	in *api.GetWirelessGatewayCertificateRequest,
) (*api.GetWirelessGatewayCertificateResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindGateway, *in.Id)
		return err
	})
	if err != nil {
		return nil, err
	}

	if r.Certificate == "" {
		return nil, missing("Certificate association of the "+kindGateway, r.ID)
	}

	return &api.GetWirelessGatewayCertificateResponse{IotCertificateId: new(r.Certificate)}, nil
}

// GetWirelessGatewayStatistics reports the last uplink a gateway took part in. No gateway connects to TinyStack, so a
// gateway counts as connected once a simulated uplink has named it.
func (s *Service) GetWirelessGatewayStatistics(
	ctx context.Context,
	in *api.GetWirelessGatewayStatisticsRequest,
) (*api.GetWirelessGatewayStatisticsResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindGateway, *in.WirelessGatewayId)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetWirelessGatewayStatisticsResponse{
		WirelessGatewayId: new(r.ID),
		ConnectionStatus:  new(statusDisconnect),
	}

	if r.LastUplink != "" {
		out.LastUplinkReceivedAt, out.ConnectionStatus = new(r.LastUplink), new(statusConnected)
	}

	return out, nil
}
