package mediaconnect

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
)

func convertSource(in *api.SetSourceRequest) api.Source {
	return api.Source{
		Decryption:        in.Decryption,
		Description:       in.Description,
		EntitlementArn:    in.EntitlementArn,
		IngestPort:        in.IngestPort,
		Name:              in.Name,
		SenderControlPort: in.SenderControlPort,
		SenderIpAddress:   in.SenderIpAddress,
		VpcInterfaceName:  in.VpcInterfaceName,
		WhitelistCidr:     in.WhitelistCidr,
		Transport: &api.Transport{
			MaxBitrate:            in.MaxBitrate,
			MaxLatency:            in.MaxLatency,
			MaxSyncBuffer:         in.MaxSyncBuffer,
			MinLatency:            in.MinLatency,
			Protocol:              in.Protocol,
			SenderControlPort:     in.SenderControlPort,
			SenderIpAddress:       in.SenderIpAddress,
			SourceListenerAddress: in.SourceListenerAddress,
			SourceListenerPort:    in.SourceListenerPort,
			StreamId:              in.StreamId,
		},
	}
}

func convertOutput(in *api.AddOutputRequest) api.Output {
	return api.Output{
		Description:            in.Description,
		Destination:            in.Destination,
		Encryption:             in.Encryption,
		Name:                   in.Name,
		Port:                   in.Port,
		VpcInterfaceAttachment: in.VpcInterfaceAttachment,
		OutputStatus:           in.OutputStatus,
		Transport: &api.Transport{
			CidrAllowList:     in.CidrAllowList,
			MaxLatency:        in.MaxLatency,
			MinLatency:        in.MinLatency,
			Protocol:          in.Protocol,
			RemoteId:          in.RemoteId,
			SenderControlPort: in.SenderControlPort,
			SmoothingLatency:  in.SmoothingLatency,
			StreamId:          in.StreamId,
		},
	}
}

func convertEntitlement(in *api.GrantEntitlementRequest) api.Entitlement {
	return api.Entitlement{
		DataTransferSubscriberFeePercent: in.DataTransferSubscriberFeePercent,
		Description:                      in.Description,
		Encryption:                       in.Encryption,
		EntitlementStatus:                in.EntitlementStatus,
		Name:                             in.Name,
		Subscribers:                      in.Subscribers,
	}
}

func convertVpcInterface(in *api.VpcInterfaceRequest) api.VpcInterface {
	return api.VpcInterface{
		Name:                 in.Name,
		NetworkInterfaceType: in.NetworkInterfaceType,
		RoleArn:              in.RoleArn,
		SecurityGroupIds:     in.SecurityGroupIds,
		SubnetId:             in.SubnetId,
	}
}

func patchSource(r *api.Source, in *api.UpdateFlowSourceRequest) {
	if in.Decryption != nil {
		if r.Decryption == nil {
			r.Decryption = &api.Encryption{}
		}

		patchEncryption(r.Decryption, in.Decryption)
	}

	if in.Description != nil {
		r.Description = in.Description
	}

	if in.EntitlementArn != nil {
		r.EntitlementArn = in.EntitlementArn
	}

	if in.IngestPort != nil {
		r.IngestPort = in.IngestPort
	}

	if in.MaxBitrate != nil {
		r.Transport.MaxBitrate = in.MaxBitrate
	}

	if in.MaxLatency != nil {
		r.Transport.MaxLatency = in.MaxLatency
	}

	if in.MaxSyncBuffer != nil {
		r.Transport.MaxSyncBuffer = in.MaxSyncBuffer
	}

	if in.MinLatency != nil {
		r.Transport.MinLatency = in.MinLatency
	}

	if in.Protocol != nil {
		r.Transport.Protocol = in.Protocol
	}

	if in.SenderControlPort != nil {
		r.SenderControlPort = in.SenderControlPort
	}

	if in.SenderControlPort != nil {
		r.Transport.SenderControlPort = in.SenderControlPort
	}

	if in.SenderIpAddress != nil {
		r.SenderIpAddress = in.SenderIpAddress
	}

	if in.SenderIpAddress != nil {
		r.Transport.SenderIpAddress = in.SenderIpAddress
	}

	if in.SourceListenerAddress != nil {
		r.Transport.SourceListenerAddress = in.SourceListenerAddress
	}

	if in.SourceListenerPort != nil {
		r.Transport.SourceListenerPort = in.SourceListenerPort
	}

	if in.StreamId != nil {
		r.Transport.StreamId = in.StreamId
	}

	if in.VpcInterfaceName != nil {
		r.VpcInterfaceName = in.VpcInterfaceName
	}

	if in.WhitelistCidr != nil {
		r.WhitelistCidr = in.WhitelistCidr
	}
}

func patchOutput(r *api.Output, in *api.UpdateFlowOutputRequest) {
	if in.CidrAllowList != nil {
		r.Transport.CidrAllowList = in.CidrAllowList
	}

	if in.Description != nil {
		r.Description = in.Description
	}

	if in.Destination != nil {
		r.Destination = in.Destination
	}

	if in.Encryption != nil {
		if r.Encryption == nil {
			r.Encryption = &api.Encryption{}
		}

		patchEncryption(r.Encryption, in.Encryption)
	}

	if in.MaxLatency != nil {
		r.Transport.MaxLatency = in.MaxLatency
	}

	if in.MinLatency != nil {
		r.Transport.MinLatency = in.MinLatency
	}

	if in.Port != nil {
		r.Port = in.Port
	}

	if in.Protocol != nil {
		r.Transport.Protocol = in.Protocol
	}

	if in.RemoteId != nil {
		r.Transport.RemoteId = in.RemoteId
	}

	if in.SenderControlPort != nil {
		r.Transport.SenderControlPort = in.SenderControlPort
	}

	if in.SenderIpAddress != nil {
		r.Transport.SenderIpAddress = in.SenderIpAddress
	}

	if in.SmoothingLatency != nil {
		r.Transport.SmoothingLatency = in.SmoothingLatency
	}

	if in.StreamId != nil {
		r.Transport.StreamId = in.StreamId
	}

	if in.VpcInterfaceAttachment != nil {
		r.VpcInterfaceAttachment = in.VpcInterfaceAttachment
	}

	if in.OutputStatus != nil {
		r.OutputStatus = in.OutputStatus
	}
}

func patchEntitlement(r *api.Entitlement, in *api.UpdateFlowEntitlementRequest) {
	if in.Description != nil {
		r.Description = in.Description
	}

	if in.Encryption != nil {
		if r.Encryption == nil {
			r.Encryption = &api.Encryption{}
		}

		patchEncryption(r.Encryption, in.Encryption)
	}

	if in.EntitlementStatus != nil {
		r.EntitlementStatus = in.EntitlementStatus
	}

	if in.Subscribers != nil {
		r.Subscribers = in.Subscribers
	}
}

func patchEncryption(r *api.Encryption, in *api.UpdateEncryption) {
	if in.Algorithm != nil {
		r.Algorithm = in.Algorithm
	}

	if in.ConstantInitializationVector != nil {
		r.ConstantInitializationVector = in.ConstantInitializationVector
	}

	if in.DeviceId != nil {
		r.DeviceId = in.DeviceId
	}

	if in.KeyType != nil {
		r.KeyType = in.KeyType
	}

	if in.Region != nil {
		r.Region = in.Region
	}

	if in.ResourceId != nil {
		r.ResourceId = in.ResourceId
	}

	if in.RoleArn != nil {
		r.RoleArn = in.RoleArn
	}

	if in.SecretArn != nil {
		r.SecretArn = in.SecretArn
	}

	if in.Url != nil {
		r.Url = in.Url
	}
}
