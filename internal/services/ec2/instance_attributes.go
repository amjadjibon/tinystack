package ec2

import (
	"context"
	"encoding/base64"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	attrBlockDevices = "blockDeviceMapping"
	attrNoStop       = "disableApiStop"
	attrNoTerminate  = "disableApiTermination"
	attrSourceDest   = "sourceDestCheck"
	attrGroups       = "groupSet"
	attrInstanceType = "instanceType"
	attrShutdown     = "instanceInitiatedShutdownBehavior"
	attrUserData     = "userData"
)

func (i *instance) deviceMappings() []api.InstanceBlockDeviceMapping {
	var out []api.InstanceBlockDeviceMapping
	for j := range i.Volumes {
		out = append(out, i.Volumes[j].instanceOutput())
	}

	return out
}

func readVisibleInstance(tx storage.ReadTx, scope request.Scope, id string, s *Service) (*instance, error) {
	i, found, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
	if err == nil && (!found || !i.visible(s.now())) {
		err = instanceNotFound(id)
	}

	return i, err
}

func (s *Service) DescribeInstanceAttribute(
	ctx context.Context, in *api.DescribeInstanceAttributeRequest,
) (*api.InstanceAttribute, error) {
	out := &api.InstanceAttribute{InstanceId: in.InstanceId}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		i, err := readVisibleInstance(tx, scope, common.Value(in.InstanceId), s)
		if err != nil {
			return err
		}

		switch common.Value(in.Attribute) {
		case attrBlockDevices:
			out.BlockDeviceMappings = i.deviceMappings()
		case attrNoStop:
			out.DisableApiStop = attribute(i.NoStop)
		case attrNoTerminate:
			out.DisableApiTermination = attribute(i.NoTerminate)
		case attrSourceDest, attrGroups:
			item := instanceOutput(scope, i, i.State)
			if fillErr := fillInstanceInterfaces(tx, scope, &item); fillErr != nil {
				return fillErr
			}

			if *in.Attribute == attrSourceDest {
				out.SourceDestCheck = &api.AttributeBooleanValue{Value: item.SourceDestCheck}
			} else {
				out.Groups = item.SecurityGroups
			}
		case attrInstanceType:
			out.InstanceType = &api.AttributeValue{Value: new(i.Type)}
		case attrShutdown:
			out.InstanceInitiatedShutdownBehavior = &api.AttributeValue{Value: new(i.Shutdown)}
		case attrUserData:
			out.UserData = &api.AttributeValue{Value: new(i.UserData)}
		case "rootDeviceName":
			out.RootDeviceName = &api.AttributeValue{Value: new(i.RootDevice)}
		case "enaSupport":
			out.EnaSupport = attribute(!i.NoENA)
		case "ebsOptimized":
			out.EbsOptimized = attribute(false)
		case attrProductCodes:
			out.ProductCodes = []api.ProductCode{}
		default:
			return invalidParameter("Unsupported instance attribute: " + common.Value(in.Attribute))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ModifyInstanceAttribute(
	ctx context.Context, in *api.ModifyInstanceAttributeRequest,
) (*api.ModifyInstanceAttributeOutput, error) {
	attr, attrErr := instanceAttributeName(in)
	if attrErr != nil {
		return nil, attrErr
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		i, err := readVisibleInstance(tx, scope, common.Value(in.InstanceId), s)
		if err != nil {
			return err
		}

		if i.State != stateRunning && i.State != stateStopped {
			return incorrectState(i.ID, "modified")
		}

		if modifyErr := s.modifyInstanceAttribute(tx, scope, i, attr, in); modifyErr != nil {
			return modifyErr
		}

		return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyInstanceAttributeOutput{}, nil
}

func instanceAttributeName(in *api.ModifyInstanceAttributeRequest) (string, error) {
	fields := map[string]bool{
		attrNoStop: in.DisableApiStop != nil, attrNoTerminate: in.DisableApiTermination != nil,
		attrSourceDest: in.SourceDestCheck != nil, attrInstanceType: in.InstanceType != nil,
		attrUserData: in.UserData != nil, attrShutdown: in.InstanceInitiatedShutdownBehavior != nil,
		attrGroups: in.Groups != nil, attrBlockDevices: in.BlockDeviceMappings != nil,
	}

	name := ""
	for field, present := range fields {
		if !present {
			continue
		}

		if name != "" {
			return "", invalidParameter("Specify exactly one instance attribute to modify")
		}

		name = field
	}

	if in.Value != nil {
		if name != "" || in.Attribute == nil {
			return "", invalidParameter("Specify both Attribute and Value or one typed attribute")
		}

		name = *in.Attribute
	}

	if name == "" || (in.Attribute != nil && *in.Attribute != name) {
		return "", invalidParameter("Specify exactly one matching instance attribute to modify")
	}

	return name, nil
}

func instanceAttributeBool(old *string, typed *api.AttributeBooleanValue) (bool, error) {
	switch {
	case old != nil:
		if value, err := strconv.ParseBool(*old); err == nil {
			return value, nil
		}
	case typed != nil && typed.Value != nil:
		return *typed.Value, nil
	}

	return false, invalidParameter("The attribute must have a boolean value")
}

func instanceAttributeString(old *string, typed *api.AttributeValue) string {
	if old != nil {
		return *old
	}

	if typed == nil {
		return ""
	}

	return common.Value(typed.Value)
}

func (s *Service) modifyInstanceAttribute(
	tx storage.WriteTx, scope request.Scope, i *instance, attr string, in *api.ModifyInstanceAttributeRequest,
) error {
	switch attr {
	case attrNoStop, attrNoTerminate, attrSourceDest:
		return modifyInstanceBoolean(tx, scope, i, attr, in)
	case attrInstanceType:
		return s.modifyInstanceType(i, instanceAttributeString(in.Value, in.InstanceType))
	case attrShutdown:
		value := instanceAttributeString(in.Value, in.InstanceInitiatedShutdownBehavior)
		if value != shutdownStop && value != shutdownTerminate {
			return invalidParameter("Instance shutdown behavior must be stop or terminate")
		}

		i.Shutdown = value
	case attrUserData:
		return modifyInstanceUserData(i, in)
	case attrGroups:
		if len(in.Groups) == 0 {
			return invalidParameter("Specify at least one security group ID")
		}

		groups, err := launchGroups(tx, scope, &subnet{ID: i.SubnetID, VPCID: i.VPCID}, in.Groups, nil)
		if err != nil {
			return err
		}

		i.Groups = groups
		return changePrimaryInterface(tx, scope, i.ID, func(ni *networkInterface) {
			ni.GroupIDs = nil
			for _, g := range groups {
				ni.GroupIDs = append(ni.GroupIDs, g.ID)
			}
		})
	case attrBlockDevices:
		return modifyInstanceDevices(tx, scope, i, in.BlockDeviceMappings)
	default:
		return invalidParameter("Unsupported instance attribute: " + attr)
	}

	return nil
}

func modifyInstanceBoolean(
	tx storage.WriteTx, scope request.Scope, i *instance, attr string, in *api.ModifyInstanceAttributeRequest,
) error {
	switch attr {
	case attrNoStop:
		val, err := instanceAttributeBool(in.Value, in.DisableApiStop)
		i.NoStop = val
		return err
	case attrNoTerminate:
		val, err := instanceAttributeBool(in.Value, in.DisableApiTermination)
		i.NoTerminate = val
		return err
	default:
		val, err := instanceAttributeBool(in.Value, in.SourceDestCheck)
		if err != nil {
			return err
		}

		i.NoSourceDestCheck = !val
		return changePrimaryInterface(tx, scope, i.ID, func(ni *networkInterface) { ni.NoSourceDestCheck = !val })
	}
}

func (s *Service) modifyInstanceType(i *instance, name string) error {
	if i.State != stateStopped {
		return incorrectState(i.ID, "resized")
	}

	arch, err := instanceType(name)
	if err != nil {
		return err
	}

	if arch != i.Arch {
		return invalidParameter("Instance type architecture must match the AMI")
	}

	if s.runtime != nil && name != i.Type {
		return awserror.New("UnsupportedOperation", "Instance type changes are supported in records mode")
	}

	i.Type = name
	return nil
}

func modifyInstanceUserData(i *instance, in *api.ModifyInstanceAttributeRequest) error {
	if i.State != stateStopped {
		return incorrectState(i.ID, "updated with user data")
	}

	var data []byte
	if in.Value != nil {
		var err error
		data, err = base64.StdEncoding.DecodeString(*in.Value)
		if err != nil {
			return invalidParameter("Invalid BASE64 encoding of user data")
		}
	} else {
		data = in.UserData.Value
	}

	if len(data) > maxUserData {
		return invalidParameter("User data is limited to " + strconv.Itoa(maxUserData) + " bytes")
	}

	i.UserData = base64.StdEncoding.EncodeToString(data)
	return nil
}

func modifyInstanceDevices(
	tx storage.WriteTx, scope request.Scope, i *instance, mappings []api.InstanceBlockDeviceMappingSpecification,
) error {
	if len(mappings) == 0 {
		return invalidParameter("Specify an EBS mapping with DeleteOnTermination")
	}

	seen := map[string]bool{}
	for _, m := range mappings {
		device := common.Value(m.DeviceName)
		if device == "" || m.Ebs == nil || m.Ebs.DeleteOnTermination == nil || seen[device] {
			return invalidParameter("Specify a unique EBS device with DeleteOnTermination for each mapping")
		}

		seen[device] = true
		if err := setDeleteOnTermination(tx, scope, i, device, m.Ebs); err != nil {
			return err
		}
	}

	return nil
}

func setDeleteOnTermination(
	tx storage.WriteTx, scope request.Scope, i *instance, device string, ebs *api.EbsInstanceBlockDeviceSpecification,
) error {
	for index := range i.Volumes {
		a := &i.Volumes[index]
		if a.Device != device {
			continue
		}

		if ebs.VolumeId != nil && *ebs.VolumeId != a.VolumeID {
			return invalidParameter("VolumeId does not match the attached device")
		}

		v, err := readVolume(tx, scope, a.VolumeID)
		if err != nil {
			return err
		}

		a.DeleteOnTermination = *ebs.DeleteOnTermination
		v.Attachment = a
		return storage.Write(tx, scope.Key("ec2", kindVolume, v.ID), v)
	}

	return invalidParameter("No EBS volume is attached at " + device)
}

func changePrimaryInterface(
	tx storage.WriteTx, scope request.Scope, id string, change func(*networkInterface),
) error {
	all, err := list[networkInterface](tx, scope, kindNetworkInterface)
	if err != nil {
		return err
	}

	for _, ni := range all {
		a := ni.Attachment
		if a != nil && common.Value(a.InstanceId) == id && common.Value(a.DeviceIndex) == 0 {
			change(ni)
			return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
		}
	}

	return nil
}

func (s *Service) GetConsoleOutput(
	ctx context.Context,
	in *api.GetConsoleOutputRequest,
) (*api.GetConsoleOutputResult, error) {
	out := &api.GetConsoleOutputResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		i, err := readVisibleInstance(tx, scope, common.Value(in.InstanceId), s)
		if err != nil {
			return err
		}

		if s.runtime != nil {
			return awserror.New("UnsupportedOperation", "VM serial console capture is unavailable")
		}

		output := "TinyStack EC2 instance " + i.ID + " is " + i.State + "\n"
		out.InstanceId, out.Timestamp = new(i.ID), new(i.Changed)
		out.Output = new(base64.StdEncoding.EncodeToString([]byte(output)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
