package ec2

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindImage = "image"

type registeredImage struct {
	Tags        map[string]string `json:",omitempty"`
	Permissions []string
	Devices     []api.BlockDeviceMapping
	ID          string
	State       string
	image
	ENA bool
}

func missingImage(id string) error {
	if !strings.HasPrefix(id, "ami-") {
		return awserror.New("InvalidAMIID.Malformed", fmt.Sprintf(`Invalid id: %q (expecting "ami-...")`, id))
	}

	return awserror.New("InvalidAMIID.NotFound", fmt.Sprintf("The image id '[%s]' does not exist", id))
}

func (img *registeredImage) output() *api.Image {
	out := imageOutput("", &img.image)
	out.ImageId, out.State, out.Public = new(
		img.ID,
	), new(
		img.State,
	), new(
		slices.Contains(img.Permissions, permissionAll),
	)
	out.EnaSupport, out.BlockDeviceMappings, out.Tags = new(img.ENA), img.Devices, tagList(img.Tags)
	return &out
}

// launchableImage resolves an AMI and enforces launch permissions. The
// caller checks availability when the operation needs a usable image.
func launchableImage(tx storage.ReadTx, scope request.Scope, id string) (*registeredImage, error) {
	img, err := readAsset[registeredImage](tx, scope, kindImage, id)
	if err != nil {
		return nil, err
	}

	if img != nil {
		if !authorized(scope.AccountID, img.Owner, img.Permissions) {
			return nil, missingImage(id)
		}

		return img, nil
	}

	fixed, err := findImage(scope.Region, id)
	if err != nil {
		return nil, err
	}

	out := imageOutput(scope.Region, fixed)
	return &registeredImage{
		ID: id, State: stateAvailable, image: *fixed, ENA: true, Devices: out.BlockDeviceMappings,
		Permissions: []string{permissionAll},
	}, nil
}

func ownedImage(tx storage.ReadTx, scope request.Scope, id string) (*registeredImage, error) {
	img, found, err := storage.Read[registeredImage](tx, scope.Key("ec2", kindImage, id))
	if err == nil && !found {
		err = missingImage(id)
	}

	return img, err
}

var imageNamePattern = regexp.MustCompile(`^[a-zA-Z0-9() ./_'@-]{3,128}$`)

func (s *Service) newImage(
	tx storage.ReadTx, scope request.Scope, name, description string, tags map[string]string,
) (*registeredImage, error) {
	if !imageNamePattern.MatchString(name) || len(description) > 255 {
		return nil, invalidParameter("Invalid AMI name or description")
	}

	all, err := list[registeredImage](tx, scope, kindImage)
	if err != nil {
		return nil, err
	}

	if slices.ContainsFunc(all, func(img *registeredImage) bool { return img.Name == name }) {
		return nil, awserror.New("InvalidAMIName.Duplicate", "An AMI already has the name "+name)
	}

	return &registeredImage{
		ID: newID("ami"), State: statePending, Tags: tags,
		image: image{
			Name: name, Description: description, Owner: scope.AccountID,
			Created: s.now().UTC().Format(time.RFC3339Nano),
		},
	}, nil
}

func snapshotInUse(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	all, err := listAssets[registeredImage](tx, scope, kindImage)
	if err != nil {
		return false, err
	}

	return slices.ContainsFunc(all, func(img *registeredImage) bool {
		return slices.ContainsFunc(img.Devices, func(d api.BlockDeviceMapping) bool {
			return common.Value(d.Ebs.SnapshotId) == id
		})
	}), nil
}

func (s *Service) CreateImage(ctx context.Context, in *api.CreateImageRequest) (*api.CreateImageResult, error) {
	tags, tagErr := splitResourceTags(in.TagSpecifications, kindImage, kindSnapshot)
	if tagErr != nil {
		return nil, tagErr
	}

	out := &api.CreateImageResult{}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.InstanceId)
		i, found, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		if err != nil {
			return err
		}

		if !found {
			return instanceNotFound(id)
		}

		if i.State != stateRunning && i.State != stateStopped {
			return incorrectState(id, "imaged")
		}

		if s.runtime != nil && i.State == stateRunning && !common.Value(in.NoReboot) {
			return awserror.New("UnsupportedOperation", "CreateImage with a running VM requires NoReboot")
		}

		img, err := s.newImage(tx, scope, common.Value(in.Name), common.Value(in.Description), tags[kindImage])
		if err != nil {
			return err
		}

		img.Architecture, img.RootDevice, img.ENA, img.BootMode = i.Arch, i.RootDevice, !i.NoENA, i.BootMode
		mappings, err := imageFromAttachments(tx, scope, i, in.BlockDeviceMappings)
		if err != nil {
			return err
		}

		now := s.now().UTC()
		for _, mapping := range mappings {
			snap, err := snapshotFromVolume(tx, scope, mapping.volume, now,
				fmt.Sprintf("Created by CreateImage(%s) for %s", i.ID, img.ID), tags[kindSnapshot])
			if err != nil {
				return err
			}

			img.Devices = append(img.Devices, volumeImageMapping(mapping.device, mapping.volume, snap, mapping.delete))
		}

		out.ImageId = new(img.ID)
		return writeAsset(tx, scope, kindImage, img.ID, img)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

type imageAttachment struct {
	volume *volume
	device string
	delete bool
}

func imageFromAttachments(
	tx storage.ReadTx, scope request.Scope, i *instance, overrides []api.BlockDeviceMapping,
) ([]imageAttachment, error) {
	byDevice := make(map[string]api.BlockDeviceMapping, len(overrides))
	for _, m := range overrides {
		device := common.Value(m.DeviceName)
		if byDevice[device].DeviceName != nil || !slices.ContainsFunc(i.Volumes, func(a volumeAttachment) bool {
			return a.Device == device
		}) {
			return nil, invalidParameter("Invalid or duplicate instance device: " + device)
		}

		if m.NoDevice != nil {
			if *m.NoDevice != "" || m.Ebs != nil || device == i.RootDevice {
				return nil, invalidParameter("NoDevice must be empty and can only suppress a non-root device")
			}
		} else if m.Ebs == nil || m.Ebs.SnapshotId != nil || m.Ebs.VolumeSize != nil || m.Ebs.VolumeType != nil ||
			m.Ebs.Encrypted != nil || m.Ebs.Iops != nil || m.Ebs.Throughput != nil || m.Ebs.DeleteOnTermination == nil {
			return nil, invalidParameter("CreateImage only supports DeleteOnTermination and NoDevice mapping overrides")
		}

		byDevice[device] = m
	}

	if !slices.ContainsFunc(i.Volumes, func(a volumeAttachment) bool { return a.Device == i.RootDevice }) {
		return nil, awserror.New("UnsupportedOperation", "The instance has no recorded root volume")
	}

	var out []imageAttachment
	for _, a := range i.Volumes {
		m := byDevice[a.Device]
		if m.NoDevice != nil {
			continue
		}

		v, err := readVolume(tx, scope, a.VolumeID)
		if err != nil {
			return nil, err
		}

		del := a.DeleteOnTermination
		if m.Ebs != nil {
			del = *m.Ebs.DeleteOnTermination
		}

		out = append(out, imageAttachment{volume: v, device: a.Device, delete: del})
	}

	return out, nil
}

func volumeImageMapping(device string, v *volume, snap *snapshot, del bool) api.BlockDeviceMapping {
	ebs := &api.EbsBlockDevice{
		VolumeSize: new(v.Size), VolumeType: new(v.Type),
		SnapshotId: new(snap.ID), Encrypted: new(snap.Encrypted), DeleteOnTermination: new(del),
	}

	if v.Type == volumeGP3 || v.Type == volumeIO1 || v.Type == volumeIO2 {
		ebs.Iops = new(v.IOPS)
	}

	if v.Type == volumeGP3 {
		ebs.Throughput = new(v.Throughput)
	}

	return api.BlockDeviceMapping{DeviceName: new(device), Ebs: ebs}
}

func (s *Service) RegisterImage(ctx context.Context, in *api.RegisterImageRequest) (*api.RegisterImageResult, error) {
	tags, tagErr := specTags(in.TagSpecifications, kindImage)
	if tagErr != nil {
		return nil, tagErr
	}

	arch := *common.OrElse(in.Architecture, new(archX86))
	if arch != archX86 && arch != archARM {
		return nil, invalidParameter("Only x86_64 and arm64 AMIs are supported")
	}

	if in.VirtualizationType != nil && *in.VirtualizationType != virtualizationHVM {
		return nil, invalidParameter("Only HVM AMIs are supported")
	}

	root := *common.OrElse(in.RootDeviceName, new(rootXVDA))
	out := &api.RegisterImageResult{}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		img, err := s.newImage(tx, scope, common.Value(in.Name), common.Value(in.Description), tags)
		if err != nil {
			return err
		}

		img.RootDevice, img.Architecture, img.ENA = root, arch, *common.OrElse(in.EnaSupport, new(true))
		img.Devices, err = registerImageDevices(tx, scope, root, in.BlockDeviceMappings, s.now().UTC())
		if err != nil {
			return err
		}

		out.ImageId = new(img.ID)
		return writeAsset(tx, scope, kindImage, img.ID, img)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func registerImageDevices(
	tx storage.ReadTx, scope request.Scope, root string, mappings []api.BlockDeviceMapping, now time.Time,
) ([]api.BlockDeviceMapping, error) {
	seen := map[string]bool{}
	var out []api.BlockDeviceMapping
	for _, m := range mappings {
		device := common.Value(m.DeviceName)
		if !deviceNamePattern.MatchString(device) || seen[device] || m.NoDevice != nil || m.Ebs == nil {
			return nil, invalidParameter("Invalid or duplicate EBS device: " + device)
		}

		seen[device] = true
		ebs := *m.Ebs
		if device == root && common.Value(ebs.SnapshotId) == "" {
			return nil, invalidParameter("The root device must specify a snapshot")
		}

		if common.Value(ebs.SnapshotId) != "" && ebs.Encrypted != nil {
			return nil, invalidParameter("Encryption cannot be specified when registering a snapshot-backed device")
		}

		v, err := newVolume(tx, scope, now, "", &ebs, nil, false)
		if err != nil {
			return nil, err
		}

		snap := &snapshot{ID: v.SnapshotID, Encrypted: v.Encrypted}
		b := volumeImageMapping(device, v, snap, *common.OrElse(ebs.DeleteOnTermination, new(device == root)))
		b.Ebs.SnapshotId = optional(snap.ID)
		out = append(out, b)
	}

	if !seen[root] {
		return nil, invalidParameter("The root device must be included in BlockDeviceMappings")
	}

	return out, nil
}

func (s *Service) DeregisterImage(
	ctx context.Context, in *api.DeregisterImageRequest,
) (*api.DeregisterImageResult, error) {
	out := &api.DeregisterImageResult{Return: new(true)}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		img, err := ownedImage(tx, scope, common.Value(in.ImageId))
		if err != nil {
			return err
		}

		deleteAsset(tx, scope, kindImage, img.ID)
		if common.Value(in.DeleteAssociatedSnapshots) {
			out.DeleteSnapshotResults, err = deleteImageSnapshots(tx, scope, img)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func deleteImageSnapshots(
	tx storage.WriteTx, scope request.Scope, img *registeredImage,
) ([]api.DeleteSnapshotReturnCode, error) {
	seen := map[string]bool{}
	var out []api.DeleteSnapshotReturnCode
	for _, device := range img.Devices {
		id := common.Value(device.Ebs.SnapshotId)
		if id == "" || seen[id] {
			continue
		}

		seen[id] = true
		code := "success"
		snap, err := readAsset[snapshot](tx, scope, kindSnapshot, id)
		if err != nil {
			return nil, err
		}

		used, err := snapshotInUse(tx, scope, id)
		if err != nil {
			return nil, err
		}

		switch {
		case snap == nil || snap.Owner != scope.AccountID:
			code = "missing-permissions"
		case used:
			code = "skipped"
		default:
			deleteAsset(tx, scope, kindSnapshot, id)
		}

		out = append(out, api.DeleteSnapshotReturnCode{SnapshotId: new(id), ReturnCode: new(code)})
	}

	return out, nil
}

func (s *Service) CopyImage(ctx context.Context, in *api.CopyImageRequest) (*api.CopyImageResult, error) {
	tags, tagErr := splitResourceTags(in.TagSpecifications, kindImage, kindSnapshot)
	if tagErr != nil {
		return nil, tagErr
	}

	out := &api.CopyImageResult{}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, "ec2", "CopyImage", in.ClientToken)
		if previous, found, replayErr := idempotency.Replay[*api.CopyImageResult](tx, token, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ")); replayErr != nil || found {
			out = previous
			return replayErr
		}

		from, err := sourceRegion(scope, in.SourceRegion)
		if err != nil {
			return err
		}

		src, err := launchableImage(tx, from, common.Value(in.SourceImageId))
		if err != nil {
			return err
		}

		if src.State != stateAvailable {
			return awserror.New("IncorrectState", "The source AMI must be available")
		}

		imageTags := map[string]string{}
		if common.Value(in.CopyImageTags) && src.Owner == scope.AccountID {
			for k, v := range src.Tags {
				if !strings.HasPrefix(k, "aws:") {
					imageTags[k] = v
				}
			}
		}

		maps.Copy(imageTags, tags[kindImage])
		if checkErr := checkTags(imageTags); checkErr != nil {
			return checkErr
		}

		img, err := s.newImage(
			tx,
			scope,
			common.Value(in.Name),
			*common.OrElse(in.Description, &src.Description),
			imageTags,
		)
		if err != nil {
			return err
		}

		img.RootDevice, img.Architecture, img.BootMode, img.ENA = src.RootDevice, src.Architecture, src.BootMode, src.ENA
		img.Devices, err = s.copyImageDevices(tx, scope, from, src, in, tags[kindSnapshot])
		if err != nil {
			return err
		}

		out.ImageId = new(img.ID)
		if writeErr := writeAsset(tx, scope, kindImage, img.ID, img); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) copyImageDevices(
	tx storage.WriteTx, scope, from request.Scope, src *registeredImage,
	in *api.CopyImageRequest, tags map[string]string,
) ([]api.BlockDeviceMapping, error) {
	devices := slices.Clone(src.Devices)
	copies := map[string]*snapshot{}
	for index := range devices {
		m := &devices[index]
		ebs := *m.Ebs
		id := common.Value(ebs.SnapshotId)
		if id == "" {
			ebs.Encrypted = new(common.Value(in.Encrypted) || common.Value(ebs.Encrypted))
			m.Ebs = &ebs
			continue
		}

		snap := copies[id]
		if snap == nil {
			source, err := snapshotForUse(tx, from, id, true)
			if err != nil {
				return nil, err
			}

			snap, err = copyVolumeSnapshot(tx, scope, source, s.now().UTC(), source.Description,
				common.Value(in.Encrypted), tags)
			if err != nil {
				return nil, err
			}

			copies[id] = snap
		}

		ebs.SnapshotId, ebs.Encrypted = new(snap.ID), new(snap.Encrypted)
		m.Ebs = &ebs
	}

	return devices, nil
}

func (s *Service) ModifyImageAttribute(
	ctx context.Context, in *api.ModifyImageAttributeRequest,
) (*api.ModifyImageAttributeOutput, error) {
	attr := common.Value(in.Attribute)
	var description *string
	var change permissionChange
	switch {
	case in.Description != nil || attr == filterDescription:
		if (attr != "" && attr != filterDescription) || in.LaunchPermission != nil ||
			in.OperationType != nil || len(in.UserIds)+len(in.UserGroups) > 0 {
			return nil, invalidParameter("Specify only the image description")
		}

		if err := onlyOne(in.Value != nil, in.Description != nil && in.Description.Value != nil); err != nil {
			return nil, err
		}

		value := common.Value(in.Value)
		if in.Description != nil && in.Description.Value != nil {
			value = *in.Description.Value
		}

		if len(value) > 255 {
			return nil, invalidParameter("AMI description is limited to 255 characters")
		}

		description = new(value)
	case attr == "" || attr == "launchPermission":
		if in.Value != nil {
			return nil, invalidParameter("Value is only supported for the description attribute")
		}

		var add, remove []api.LaunchPermission
		if p := in.LaunchPermission; p != nil {
			add, remove = p.Add, p.Remove
		}

		var err error
		change, err = parsePermissionChange(add, remove, in.UserIds, in.UserGroups, in.OperationType,
			func(p api.LaunchPermission) (*string, *string) { return p.UserId, p.Group })
		if err != nil {
			return nil, err
		}

	default:
		return nil, invalidParameter("Unsupported AMI attribute: " + attr)
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		img, err := ownedImage(tx, scope, common.Value(in.ImageId))
		if err != nil {
			return err
		}

		if description != nil {
			img.Description = *description
		} else {
			if slices.Contains(change.add, permissionAll) &&
				slices.ContainsFunc(img.Devices, func(m api.BlockDeviceMapping) bool {
					return common.Value(m.Ebs.Encrypted)
				}) {
				return awserror.New("OperationNotPermitted", "An encrypted AMI cannot be made public")
			}

			img.Permissions = change.apply(img.Permissions)
		}

		return storage.Write(tx, scope.Key("ec2", kindImage, img.ID), img)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyImageAttributeOutput{}, nil
}

func (s *Service) DescribeImageAttribute(
	ctx context.Context,
	in *api.DescribeImageAttributeRequest,
) (*api.ImageAttribute, error) {
	out := &api.ImageAttribute{ImageId: in.ImageId}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		img, err := launchableImage(tx, scope, common.Value(in.ImageId))
		if err != nil {
			return err
		}

		switch common.Value(in.Attribute) {
		case filterDescription:
			out.Description = &api.AttributeValue{Value: new(img.Description)}
		case "blockDeviceMapping":
			out.BlockDeviceMappings = img.Devices
		case "bootMode":
			out.BootMode = &api.AttributeValue{Value: optional(img.BootMode)}
		case attrProductCodes:
			out.ProductCodes = []api.ProductCode{}
		case "launchPermission":
			if img.Owner != scope.AccountID && !slices.Contains(img.Permissions, permissionAll) {
				return awserror.New("AuthFailure", "Only the AMI owner can view private launch permissions")
			}

			for _, p := range img.Permissions {
				if img.Owner != scope.AccountID && p != permissionAll {
					continue
				}

				user, group := permissionFields(p)
				out.LaunchPermissions = append(out.LaunchPermissions, api.LaunchPermission{UserId: user, Group: group})
			}
		default:
			return invalidParameter("Unsupported AMI attribute: " + common.Value(in.Attribute))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
