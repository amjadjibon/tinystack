package networkfirewall

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// subnetZones reads each subnet from EC2 within tx and returns its Availability Zone. Every subnet must be in vpcID
// and in a distinct zone, as AWS requires one firewall endpoint per zone.
func (s *Service) subnetZones(
	tx storage.ReadTx,
	scope request.Scope,
	vpcID string,
	mappings []api.SubnetMapping,
) (map[string]string, error) {
	zones, seen := map[string]string{}, map[string]string{}
	for _, m := range mappings {
		id := common.Value(m.SubnetId)
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return nil, awserror.New("InvalidRequestException", fmt.Sprintf("Subnet %s does not exist", id))
		}

		if common.Value(sn.VpcId) != vpcID {
			return nil, awserror.New("InvalidRequestException", fmt.Sprintf("Subnet %s is not in VPC %s", id, vpcID))
		}

		az := common.Value(sn.AvailabilityZone)
		if other, ok := seen[az]; ok {
			return nil, awserror.New(
				"InvalidRequestException",
				fmt.Sprintf("Multiple subnets (%s, %s) in the same Availability Zone (%s)", other, id, az),
			)
		}

		seen[az], zones[id] = id, az
	}

	return zones, nil
}

func (s *Service) CreateFirewall(
	ctx context.Context,
	in *api.CreateFirewallRequest,
) (*api.CreateFirewallResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.VpcId == nil || *in.VpcId == "" {
		return nil, awserror.New("InvalidRequestException", "VpcId is required")
	}

	if len(in.SubnetMappings) == 0 {
		return nil, awserror.New("InvalidRequestException", "At least one subnet mapping is required")
	}

	name := *in.FirewallName
	arn := firewallARN(scope, name)
	firewallKey := scope.Key(serviceName, kindFirewall, name)

	polName, pErr := resolvePolicyName(scope, nil, in.FirewallPolicyArn)
	if pErr != nil {
		return nil, pErr
	}

	policyKey := scope.Key(serviceName, kindPolicy, polName)

	firewallID := newUUID()
	updateToken := newUUID()

	fw := &api.Firewall{
		FirewallName:                   new(name),
		FirewallArn:                    new(arn),
		FirewallId:                     new(firewallID),
		FirewallPolicyArn:              in.FirewallPolicyArn,
		VpcId:                          in.VpcId,
		SubnetMappings:                 in.SubnetMappings,
		DeleteProtection:               in.DeleteProtection,
		SubnetChangeProtection:         in.SubnetChangeProtection,
		FirewallPolicyChangeProtection: in.FirewallPolicyChangeProtection,
		Description:                    in.Description,
		Tags:                           in.Tags,
		EncryptionConfiguration:        in.EncryptionConfiguration,
	}

	var resp *api.CreateFirewallResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if keyExists(tx, firewallKey) {
			return awserror.New("InvalidRequestException", fmt.Sprintf("A firewall with name %s already exists", name))
		}

		subnetAZs, zoneErr := s.subnetZones(tx, scope, *in.VpcId, in.SubnetMappings)
		if zoneErr != nil {
			return zoneErr
		}

		polRec, ok, polReadErr := storage.Read[policyRecord](tx, policyKey)
		if polReadErr != nil {
			return polReadErr
		}

		if !ok {
			return awserror.New(
				"ResourceNotFoundException",
				fmt.Sprintf("Firewall policy not found: %s", *in.FirewallPolicyArn),
			)
		}

		syncStates := make(map[string]api.SyncState)
		for _, sm := range in.SubnetMappings {
			sID := *sm.SubnetId
			az := subnetAZs[sID]
			epID := deterministicEndpointID(name, sID)
			syncStates[az] = api.SyncState{
				Attachment: &api.Attachment{
					SubnetId:   new(sID),
					EndpointId: new(epID),
					Status:     new(statusReady),
				},
			}
		}

		fwStatus := &api.FirewallStatus{
			Status:                        new(statusReady),
			ConfigurationSyncStateSummary: new(syncInSync),
			SyncStates:                    syncStates,
		}

		rec := firewallRecord{
			Firewall:       fw,
			FirewallStatus: fwStatus,
			SubnetAZs:      subnetAZs,
			UpdateToken:    updateToken,
		}

		if wErr := storage.Write(tx, firewallKey, rec); wErr != nil {
			return wErr
		}

		// Increment association count on policy
		polRec.NumberOfAssociations++
		if polRec.PolicyResponse != nil {
			polRec.PolicyResponse.NumberOfAssociations = new(polRec.NumberOfAssociations)
		}

		if wErr := storage.Write(tx, policyKey, polRec); wErr != nil {
			return wErr
		}

		resp = &api.CreateFirewallResponse{
			Firewall:       common.Clone(fw),
			FirewallStatus: common.Clone(fwStatus),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DescribeFirewall(
	ctx context.Context,
	in *api.DescribeFirewallRequest,
) (*api.DescribeFirewallResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.DescribeFirewallResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		resp = &api.DescribeFirewallResponse{
			Firewall:       common.Clone(rec.Firewall),
			FirewallStatus: common.Clone(rec.FirewallStatus),
			UpdateToken:    new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) ListFirewalls(ctx context.Context, in *api.ListFirewallsRequest) (*api.ListFirewallsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.Key(serviceName, kindFirewall, "")
	var firewalls []api.FirewallMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			rec, ok, rErr := storage.Read[firewallRecord](tx, k)
			if rErr != nil || !ok || rec.Firewall == nil {
				continue
			}

			if len(in.VpcIds) > 0 {
				match := false
				for _, v := range in.VpcIds {
					if rec.Firewall.VpcId != nil && *rec.Firewall.VpcId == v {
						match = true
						break
					}
				}

				if !match {
					continue
				}
			}

			firewalls = append(firewalls, api.FirewallMetadata{
				FirewallName: new(*rec.Firewall.FirewallName),
				FirewallArn:  new(*rec.Firewall.FirewallArn),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	limit := 100
	if in.MaxResults != nil && *in.MaxResults > 0 {
		limit = int(*in.MaxResults)
	}

	paged, next, err := common.Page(firewalls, func(f api.FirewallMetadata) string {
		return common.Value(f.FirewallName)
	}, in.NextToken, limit, awserror.New("InvalidRequestException", "Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListFirewallsResponse{
		Firewalls: paged,
		NextToken: next,
	}, nil
}

func (s *Service) DeleteFirewall(
	ctx context.Context,
	in *api.DeleteFirewallRequest,
) (*api.DeleteFirewallResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.DeleteFirewallResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if rec.Firewall.DeleteProtection != nil && *rec.Firewall.DeleteProtection {
			return awserror.New("InvalidOperationException", "Delete protection is enabled for this firewall")
		}

		// Decrement association count on policy
		if rec.Firewall.FirewallPolicyArn != nil {
			polName, pErr := resolvePolicyName(scope, nil, rec.Firewall.FirewallPolicyArn)
			if pErr == nil {
				policyKey := scope.Key(serviceName, kindPolicy, polName)
				if polRec, pOk, polReadErr := storage.Read[policyRecord](tx, policyKey); polReadErr == nil && pOk {
					if polRec.NumberOfAssociations > 0 {
						polRec.NumberOfAssociations--
					}

					if polRec.PolicyResponse != nil {
						polRec.PolicyResponse.NumberOfAssociations = new(polRec.NumberOfAssociations)
					}

					_ = storage.Write(tx, policyKey, polRec)
				}
			}
		}

		// Clean up logging configuration
		logKey := scope.Key(serviceName, kindLogging, name)
		tx.Delete(logKey)

		// Delete any attached resource policy
		arn := firewallARN(scope, name)
		resPolicyKey := scope.Key(serviceName, kindResourcePolicy, arn)
		tx.Delete(resPolicyKey)

		tx.Delete(key)

		statusCopy := common.Clone(rec.FirewallStatus)
		statusCopy.Status = new(statusDeleting)

		resp = &api.DeleteFirewallResponse{
			Firewall:       common.Clone(rec.Firewall),
			FirewallStatus: statusCopy,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateFirewallDescription(
	ctx context.Context,
	in *api.UpdateFirewallDescriptionRequest,
) (*api.UpdateFirewallDescriptionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.UpdateFirewallDescriptionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		rec.Firewall.Description = in.Description
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateFirewallDescriptionResponse{
			FirewallArn:  rec.Firewall.FirewallArn,
			FirewallName: rec.Firewall.FirewallName,
			Description:  rec.Firewall.Description,
			UpdateToken:  new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateFirewallDeleteProtection(
	ctx context.Context,
	in *api.UpdateFirewallDeleteProtectionRequest,
) (*api.UpdateFirewallDeleteProtectionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.UpdateFirewallDeleteProtectionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		rec.Firewall.DeleteProtection = in.DeleteProtection
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateFirewallDeleteProtectionResponse{
			FirewallArn:      rec.Firewall.FirewallArn,
			FirewallName:     rec.Firewall.FirewallName,
			DeleteProtection: rec.Firewall.DeleteProtection,
			UpdateToken:      new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateFirewallPolicyChangeProtection(
	ctx context.Context,
	in *api.UpdateFirewallPolicyChangeProtectionRequest,
) (*api.UpdateFirewallPolicyChangeProtectionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.UpdateFirewallPolicyChangeProtectionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		rec.Firewall.FirewallPolicyChangeProtection = in.FirewallPolicyChangeProtection
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateFirewallPolicyChangeProtectionResponse{
			FirewallArn:                    rec.Firewall.FirewallArn,
			FirewallName:                   rec.Firewall.FirewallName,
			FirewallPolicyChangeProtection: rec.Firewall.FirewallPolicyChangeProtection,
			UpdateToken:                    new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateSubnetChangeProtection(
	ctx context.Context,
	in *api.UpdateSubnetChangeProtectionRequest,
) (*api.UpdateSubnetChangeProtectionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.UpdateSubnetChangeProtectionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		rec.Firewall.SubnetChangeProtection = in.SubnetChangeProtection
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateSubnetChangeProtectionResponse{
			FirewallArn:            rec.Firewall.FirewallArn,
			FirewallName:           rec.Firewall.FirewallName,
			SubnetChangeProtection: rec.Firewall.SubnetChangeProtection,
			UpdateToken:            new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) AssociateFirewallPolicy(
	ctx context.Context,
	in *api.AssociateFirewallPolicyRequest,
) (*api.AssociateFirewallPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	newPolName, err := resolvePolicyName(scope, nil, in.FirewallPolicyArn)
	if err != nil {
		return nil, err
	}

	newPolKey := scope.Key(serviceName, kindPolicy, newPolName)

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.AssociateFirewallPolicyResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		if rec.Firewall.FirewallPolicyChangeProtection != nil && *rec.Firewall.FirewallPolicyChangeProtection {
			return awserror.New(
				"InvalidOperationException",
				"Firewall policy change protection is enabled for this firewall",
			)
		}

		newPolRec, polOk, pErr := storage.Read[policyRecord](tx, newPolKey)
		if pErr != nil {
			return pErr
		}

		if !polOk {
			return awserror.New(
				"ResourceNotFoundException",
				fmt.Sprintf("Firewall policy not found: %s", *in.FirewallPolicyArn),
			)
		}

		// Decrement old policy association count
		if rec.Firewall.FirewallPolicyArn != nil {
			oldPolName, oErr := resolvePolicyName(scope, nil, rec.Firewall.FirewallPolicyArn)
			if oErr == nil {
				oldPolKey := scope.Key(serviceName, kindPolicy, oldPolName)
				if oldPolRec, oldOk, oldErr := storage.Read[policyRecord](tx, oldPolKey); oldErr == nil && oldOk {
					if oldPolRec.NumberOfAssociations > 0 {
						oldPolRec.NumberOfAssociations--
					}

					if oldPolRec.PolicyResponse != nil {
						oldPolRec.PolicyResponse.NumberOfAssociations = new(oldPolRec.NumberOfAssociations)
					}

					_ = storage.Write(tx, oldPolKey, oldPolRec)
				}
			}
		}

		// Increment new policy association count
		newPolRec.NumberOfAssociations++
		if newPolRec.PolicyResponse != nil {
			newPolRec.PolicyResponse.NumberOfAssociations = new(newPolRec.NumberOfAssociations)
		}

		if wErr := storage.Write(tx, newPolKey, newPolRec); wErr != nil {
			return wErr
		}

		rec.Firewall.FirewallPolicyArn = in.FirewallPolicyArn
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.AssociateFirewallPolicyResponse{
			FirewallArn:       rec.Firewall.FirewallArn,
			FirewallName:      rec.Firewall.FirewallName,
			FirewallPolicyArn: rec.Firewall.FirewallPolicyArn,
			UpdateToken:       new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) AssociateSubnets(
	ctx context.Context,
	in *api.AssociateSubnetsRequest,
) (*api.AssociateSubnetsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	if len(in.SubnetMappings) == 0 {
		return nil, awserror.New("InvalidRequestException", "At least one subnet mapping is required")
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.AssociateSubnetsResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		if common.Value(rec.Firewall.SubnetChangeProtection) {
			return awserror.New("InvalidOperationException", "Subnet change protection is enabled for this firewall")
		}

		newSubnetAZs, zoneErr := s.subnetZones(tx, scope, common.Value(rec.Firewall.VpcId), in.SubnetMappings)
		if zoneErr != nil {
			return zoneErr
		}

		if rec.FirewallStatus.SyncStates == nil {
			rec.FirewallStatus.SyncStates = make(map[string]api.SyncState)
		}

		if rec.SubnetAZs == nil {
			rec.SubnetAZs = make(map[string]string)
		}

		// Ensure no AZ conflict
		for _, sm := range in.SubnetMappings {
			sID := *sm.SubnetId
			az := newSubnetAZs[sID]
			if _, exists := rec.FirewallStatus.SyncStates[az]; exists {
				return awserror.New(
					"InvalidRequestException",
					fmt.Sprintf("Firewall already has a subnet in Availability Zone %s", az),
				)
			}
		}

		for _, sm := range in.SubnetMappings {
			sID := *sm.SubnetId
			az := newSubnetAZs[sID]
			epID := deterministicEndpointID(name, sID)
			rec.FirewallStatus.SyncStates[az] = api.SyncState{
				Attachment: &api.Attachment{
					SubnetId:   new(sID),
					EndpointId: new(epID),
					Status:     new(statusReady),
				},
			}

			rec.Firewall.SubnetMappings = append(rec.Firewall.SubnetMappings, sm)
			rec.SubnetAZs[sID] = az
		}

		rec.UpdateToken = newUUID()
		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.AssociateSubnetsResponse{
			FirewallArn:    rec.Firewall.FirewallArn,
			FirewallName:   rec.Firewall.FirewallName,
			SubnetMappings: rec.Firewall.SubnetMappings,
			UpdateToken:    new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DisassociateSubnets(
	ctx context.Context,
	in *api.DisassociateSubnetsRequest,
) (*api.DisassociateSubnetsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	if len(in.SubnetIds) == 0 {
		return nil, awserror.New("InvalidRequestException", "At least one subnet ID is required")
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.DisassociateSubnetsResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		if rec.Firewall.SubnetChangeProtection != nil && *rec.Firewall.SubnetChangeProtection {
			return awserror.New("InvalidOperationException", "Subnet change protection is enabled for this firewall")
		}

		toRemove := make(map[string]bool, len(in.SubnetIds))
		for _, id := range in.SubnetIds {
			toRemove[id] = true
		}

		newMappings := make([]api.SubnetMapping, 0, len(rec.Firewall.SubnetMappings))
		for _, sm := range rec.Firewall.SubnetMappings {
			if sm.SubnetId != nil && toRemove[*sm.SubnetId] {
				az := rec.SubnetAZs[*sm.SubnetId]
				delete(rec.FirewallStatus.SyncStates, az)
				delete(rec.SubnetAZs, *sm.SubnetId)
			} else {
				newMappings = append(newMappings, sm)
			}
		}

		rec.Firewall.SubnetMappings = newMappings
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.DisassociateSubnetsResponse{
			FirewallArn:    rec.Firewall.FirewallArn,
			FirewallName:   rec.Firewall.FirewallName,
			SubnetMappings: rec.Firewall.SubnetMappings,
			UpdateToken:    new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateFirewallEncryptionConfiguration(
	ctx context.Context,
	in *api.UpdateFirewallEncryptionConfigurationRequest,
) (*api.UpdateFirewallEncryptionConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindFirewall, name)
	var resp *api.UpdateFirewallEncryptionConfigurationResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[firewallRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		rec.Firewall.EncryptionConfiguration = in.EncryptionConfiguration
		rec.UpdateToken = newUUID()

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateFirewallEncryptionConfigurationResponse{
			FirewallArn:             rec.Firewall.FirewallArn,
			FirewallName:            rec.Firewall.FirewallName,
			EncryptionConfiguration: rec.Firewall.EncryptionConfiguration,
			UpdateToken:             new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}
