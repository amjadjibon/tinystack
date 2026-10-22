package ec2

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// InstanceMetadata is the EC2-owned view used by the metadata listener.
type InstanceMetadata struct {
	LaunchTime      time.Time
	Scope           request.Scope
	ID              string
	Type            string
	ImageID         string
	PrivateIP       string
	PublicIP        string
	Zone            string
	SubnetID        string
	VPCID           string
	State           string
	UserData        string
	MAC             string
	InterfaceID     string
	VPCCIDR         string
	ProfileARN      string
	ProfileID       string
	TokensRequired  bool
	EndpointEnabled bool
}

// MetadataInstance resolves an ID across partitions, or the single running instance.
// A missing or ambiguous selection returns found=false.
func (s *Service) MetadataInstance(ctx context.Context, id string) (*InstanceMetadata, bool, error) {
	var result *InstanceMetadata
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var keys []string
		if id != "" {
			keys = []string{"aws/ec2/instances/" + id}
		} else {
			keys = tx.Keys("aws/ec2/instances/")
		}

		for _, key := range keys {
			scope, found, err := storage.Read[request.Scope](tx, key)
			if err != nil {
				return err
			}

			if !found {
				continue
			}

			instanceID := strings.TrimPrefix(key, "aws/ec2/instances/")
			i, found, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, instanceID))
			if err != nil {
				return err
			}

			if !found || !i.visible(s.now().UTC()) || (id == "" && i.State != stateRunning) {
				continue
			}

			if result != nil {
				result = nil
				return nil
			}

			result = &InstanceMetadata{
				LaunchTime: i.LaunchTime, EndpointEnabled: true,
				Scope: *scope, ID: i.ID, Type: i.Type, ImageID: i.ImageID, PrivateIP: i.PrivateIP, PublicIP: i.PublicIP,
				Zone: i.Zone, SubnetID: i.SubnetID, VPCID: i.VPCID, State: i.State, UserData: i.UserData,
			}

			if i.MetadataOptions != nil {
				result.TokensRequired = i.MetadataOptions.HttpTokens == "required"
				result.EndpointEnabled = i.MetadataOptions.HttpEndpoint != metadataDisabled
			}

			if i.Profile != nil {
				result.ProfileARN = i.Profile.ARN
				result.ProfileID = i.Profile.ProfileID
			}

			interfaces, err := list[networkInterface](tx, *scope, kindNetworkInterface)
			if err != nil {
				return err
			}

			for _, eni := range interfaces {
				if eni.Attachment != nil && common.Value(eni.Attachment.InstanceId) == i.ID {
					result.InterfaceID = eni.ID
					break
				}
			}

			result.MAC = macAddress(result.InterfaceID)
			if i.VPCID != "" {
				v, err := loadVPC(tx, *scope, i.VPCID)
				if err != nil {
					return err
				}

				result.VPCCIDR = v.CIDR
			}
		}

		return nil
	})

	return result, result != nil, err
}
