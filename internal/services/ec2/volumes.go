package ec2

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	volumeIO1        = "io1"
	volumeIO2        = "io2"
	volumeST1        = "st1"
	volumeSC1        = "sc1"
	volumeStandard   = "standard"
	kindVolume       = "volume"
	stateCreating    = "creating"
	filterVolumeID   = "volume-id"
	filterSnapshotID = "snapshot-id"
	filterVolumeType = "volume-type"
	filterEncrypted  = "encrypted"
)

type volumeConfig struct {
	Type       string
	Size       int64
	IOPS       int64
	Throughput int64
}

type volume struct {
	Created      time.Time
	Attachment   *volumeAttachment
	Modification *api.VolumeModification
	Tags         map[string]string `json:",omitempty"`
	ID           string
	Zone         string
	State        string
	SnapshotID   string
	volumeConfig
	Encrypted bool
}

type volumeAttachment struct {
	Since               time.Time
	VolumeID            string
	InstanceID          string
	Device              string
	DeleteOnTermination bool
}

func volumeNotFound(id string) error { return notFound("InvalidVolume.NotFound", kindVolume, id) }

func readVolume(tx storage.ReadTx, scope request.Scope, id string) (*volume, error) {
	v, ok, err := storage.Read[volume](tx, scope.Key("ec2", kindVolume, id))
	if err == nil && !ok {
		err = volumeNotFound(id)
	}

	return v, err
}

func (a *volumeAttachment) output(state string) *api.VolumeAttachment {
	return &api.VolumeAttachment{
		VolumeId: new(a.VolumeID), InstanceId: new(a.InstanceID), Device: new(a.Device), State: new(state),
		AttachTime: new(a.Since), DeleteOnTermination: new(a.DeleteOnTermination),
	}
}

func (a *volumeAttachment) instanceOutput() api.InstanceBlockDeviceMapping {
	return api.InstanceBlockDeviceMapping{DeviceName: new(a.Device), Ebs: &api.EbsInstanceBlockDevice{
		VolumeId: new(a.VolumeID), AttachTime: new(a.Since), Status: new(stateAttached),
		DeleteOnTermination: new(a.DeleteOnTermination),
	}}
}

func (v *volume) output(scope request.Scope) *api.Volume {
	out := &api.Volume{
		VolumeId: new(v.ID), Size: new(v.Size), SnapshotId: new(v.SnapshotID), State: new(v.State),
		CreateTime: new(v.Created), AvailabilityZone: new(v.Zone), VolumeType: new(v.Type),
		AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(v.Zone, scope.Region))),
		Encrypted:          new(v.Encrypted), Tags: tagList(v.Tags),
	}

	if v.IOPS > 0 {
		out.Iops = new(v.IOPS)
	}

	if v.Throughput > 0 {
		out.Throughput = new(v.Throughput)
	}

	if v.Attachment != nil {
		out.Attachments = []api.VolumeAttachment{*v.Attachment.output(stateAttached)}
	}

	return out
}

func (v *volume) values(scope request.Scope) values {
	vals := values{
		filterVolumeID: {v.ID}, filterZone: {v.Zone},
		filterZoneID: {zoneID(scope.Region, strings.TrimPrefix(v.Zone, scope.Region))},
		"size": {
			strconv.FormatInt(v.Size, 10),
		}, filterVolumeType: {v.Type}, filterEncrypted: boolValue(v.Encrypted),
		filterStatus: {v.State}, filterSnapshotID: {v.SnapshotID}, "create-time": {v.Created.Format(time.RFC3339)},
		"attachment.device": {}, filterAttachmentInstanceID: {}, "attachment.status": {}, "attachment.attach-time": {},
		"attachment.delete-on-termination": {}, "multi-attach-enabled": boolValue(false),
	}

	if a := v.Attachment; a != nil {
		vals["attachment.device"] = []string{a.Device}
		vals[filterAttachmentInstanceID] = []string{a.InstanceID}
		vals["attachment.status"] = []string{stateAttached}
		vals["attachment.attach-time"] = []string{a.Since.Format(time.RFC3339)}
		vals["attachment.delete-on-termination"] = boolValue(a.DeleteOnTermination)
	}

	return vals.withTags(v.Tags)
}

func resolveVolumeConfig(typ string, size, iops, throughput *int64) (volumeConfig, error) {
	config := volumeConfig{
		Type: typ, Size: common.Value(size), IOPS: common.Value(iops),
		Throughput: common.Value(throughput),
	}

	var minimum, maximum int64
	switch typ {
	case volumeGP3:
		minimum, maximum = 1, 65536
		config.IOPS = *common.OrElse(iops, new(int64(3000)))
		config.Throughput = *common.OrElse(throughput, new(int64(125)))
		if config.IOPS < 3000 || config.IOPS > 80000 || config.Throughput < 125 ||
			config.Throughput > 2000 || config.Throughput > config.IOPS/4 {
			return config, invalidParameter("Invalid IOPS or throughput for gp3")
		}
	case "gp2":
		minimum, maximum = 1, 16384
		if iops != nil {
			return config, invalidParameter("IOPS cannot be specified for gp2")
		}

		config.IOPS = min(max(100, config.Size*3), 16000)
	case volumeIO1, volumeIO2:
		minimum, maximum = 4, 16384
		if typ == volumeIO2 {
			maximum = 65536
		}

		if config.IOPS < 100 || config.IOPS > 256000 {
			return config, invalidParameter("Provisioned IOPS are required for io1 and io2")
		}
	case volumeST1, volumeSC1:
		minimum, maximum = 125, 16384
	case volumeStandard:
		minimum, maximum = 1, 1024
	default:
		return config, invalidParameter("Unsupported volume type: " + typ)
	}

	if config.Size < minimum || config.Size > maximum {
		return config, invalidParameter(fmt.Sprintf("Size for %s must be between %d and %d GiB", typ, minimum, maximum))
	}

	if throughput != nil && typ != volumeGP3 {
		return config, invalidParameter("Throughput can only be specified for gp3")
	}

	if iops != nil && (typ == volumeST1 || typ == volumeSC1 || typ == volumeStandard) {
		return config, invalidParameter("IOPS cannot be specified for " + typ)
	}

	return config, nil
}

func newVolume(
	tx storage.ReadTx, scope request.Scope, now time.Time, az string, ebs *api.EbsBlockDevice, tags map[string]string,
	trustedSnapshot bool,
) (*volume, error) {
	v := &volume{
		ID: newID("vol"), Created: now, Zone: az, State: stateCreating, Tags: tags,
		SnapshotID: common.Value(ebs.SnapshotId),
	}

	defaultEncryption, _, err := storage.Read[bool](tx, scope.Key("ec2", "settings", "ebs-encryption"))
	if err != nil {
		return nil, err
	}

	v.Encrypted = common.Value(ebs.Encrypted) || (defaultEncryption != nil && *defaultEncryption)
	size := ebs.VolumeSize
	if v.SnapshotID != "" {
		snap, snapErr := snapshotForUse(tx, scope, v.SnapshotID, trustedSnapshot)
		if snapErr != nil {
			return nil, snapErr
		}

		if size == nil {
			size = new(snap.Size)
		}

		if *size < snap.Size {
			return nil, invalidParameter("Volume size cannot be smaller than its snapshot")
		}

		v.Encrypted = v.Encrypted || snap.Encrypted
	}

	if size == nil {
		return nil, awserror.New("MissingParameter", "Specify Size or SnapshotId")
	}

	v.volumeConfig, err = resolveVolumeConfig(
		*common.OrElse(ebs.VolumeType, new("gp2")),
		size,
		ebs.Iops,
		ebs.Throughput,
	)
	return v, err
}

func (s *Service) CreateVolume(ctx context.Context, in *api.CreateVolumeRequest) (*api.Volume, error) {
	tags, err := specTags(in.TagSpecifications, kindVolume)
	if err != nil {
		return nil, err
	}

	az, azID := common.Value(in.AvailabilityZone), common.Value(in.AvailabilityZoneId)
	if az == "" && azID == "" {
		return nil, awserror.New("MissingParameter", "Specify AvailabilityZone or AvailabilityZoneId")
	}

	if az != "" && azID != "" {
		return nil, invalidParameter("Specify one of AvailabilityZone and AvailabilityZoneId")
	}

	var out *api.Volume
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		k := idempotency.Key(scope, "ec2", "CreateVolume", in.ClientToken)
		if prev, found, replayErr := idempotency.Replay[*api.Volume](tx, k, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ")); replayErr != nil || found {
			out = prev
			return replayErr
		}

		placement, zoneErr := zone(scope.Region, az, azID)
		if zoneErr != nil {
			return zoneErr
		}

		v, createErr := newVolume(tx, scope, s.now().UTC(), placement, &api.EbsBlockDevice{
			SnapshotId: in.SnapshotId, VolumeSize: in.Size, VolumeType: in.VolumeType,
			Iops: in.Iops, Throughput: in.Throughput, Encrypted: in.Encrypted,
		}, tags, false)
		if createErr != nil {
			return createErr
		}

		if writeErr := writePendingStorage(tx, scope, kindVolume, v.ID, v); writeErr != nil {
			return writeErr
		}

		out = v.output(scope)
		return idempotency.Remember(tx, k, in, out)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteVolume(ctx context.Context, in *api.DeleteVolumeRequest) (*api.DeleteVolumeOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := readVolume(tx, scope, common.Value(in.VolumeId))
		if err != nil {
			return err
		}

		if v.Attachment != nil {
			return awserror.New("VolumeInUse", "The volume is attached to an instance")
		}

		deleteVolume(tx, scope, v.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVolumeOutput{}, nil
}

func deleteVolume(tx storage.WriteTx, scope request.Scope, id string) {
	tx.Delete(scope.Key("ec2", kindVolume, id))
	tx.Delete(storageTransitionKey(scope, kindVolume, id))
}

var deviceNamePattern = regexp.MustCompile(`^/dev/(sd|xvd|hd)[a-z]+\d*$`)

func (s *Service) AttachVolume(ctx context.Context, in *api.AttachVolumeRequest) (*api.VolumeAttachment, error) {
	device := common.Value(in.Device)
	if !deviceNamePattern.MatchString(device) {
		return nil, invalidParameter("Invalid EBS device name: " + device)
	}

	var out *api.VolumeAttachment
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := readVolume(tx, scope, common.Value(in.VolumeId))
		if err != nil {
			return err
		}

		id := common.Value(in.InstanceId)
		i, found, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		if err != nil {
			return err
		}

		switch {
		case !found:
			return instanceNotFound(id)
		case i.State != stateRunning && i.State != stateStopped:
			return incorrectState(id, "attached to")
		case i.Zone != v.Zone:
			return awserror.New(
				"InvalidVolume.ZoneMismatch",
				"Volume and instance must be in the same availability zone",
			)
		case v.Attachment != nil:
			return awserror.New("VolumeInUse", "The volume is attached to an instance")
		case v.State != stateAvailable:
			return awserror.New("IncorrectState", "The volume must be available")
		case slices.ContainsFunc(i.Volumes, func(a volumeAttachment) bool { return a.Device == device }):
			return invalidParameter("The device is already in use: " + device)
		}

		a := volumeAttachment{VolumeID: v.ID, InstanceID: id, Device: device, Since: s.now().UTC()}
		v.Attachment, v.State = new(a), ec2InUse
		i.Volumes = append(i.Volumes, a)
		if writeErr := storage.Write(tx, scope.Key("ec2", kindInstance, id), i); writeErr != nil {
			return writeErr
		}

		out = a.output(stateAttached)
		return storage.Write(tx, scope.Key("ec2", kindVolume, v.ID), v)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DetachVolume(ctx context.Context, in *api.DetachVolumeRequest) (*api.VolumeAttachment, error) {
	var out *api.VolumeAttachment
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := readVolume(tx, scope, common.Value(in.VolumeId))
		if err != nil {
			return err
		}

		a := v.Attachment
		if a == nil || (in.InstanceId != nil && *in.InstanceId != a.InstanceID) ||
			(in.Device != nil && *in.Device != a.Device) {
			return awserror.New("InvalidAttachment.NotFound", "No matching volume attachment")
		}

		k := scope.Key("ec2", kindInstance, a.InstanceID)
		i, found, err := storage.Read[instance](tx, k)
		if err != nil {
			return err
		}

		if found {
			if a.Device == i.RootDevice && i.State != stateStopped {
				return incorrectState(i.ID, "detached from its root volume")
			}

			i.Volumes = slices.DeleteFunc(i.Volumes, func(item volumeAttachment) bool { return item.VolumeID == v.ID })
			if writeErr := storage.Write(tx, k, i); writeErr != nil {
				return writeErr
			}
		}

		v.Attachment, v.State = nil, stateAvailable
		out = a.output("detached")
		return storage.Write(tx, scope.Key("ec2", kindVolume, v.ID), v)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func releaseInstanceVolumes(tx storage.WriteTx, scope request.Scope, i *instance) error {
	for _, a := range i.Volumes {
		if a.DeleteOnTermination {
			deleteVolume(tx, scope, a.VolumeID)
			continue
		}

		v, err := readVolume(tx, scope, a.VolumeID)
		if err != nil {
			return err
		}

		v.Attachment, v.State = nil, stateAvailable
		if writeErr := storage.Write(tx, scope.Key("ec2", kindVolume, v.ID), v); writeErr != nil {
			return writeErr
		}
	}

	i.Volumes = nil
	return nil
}

func (s *Service) DescribeVolumes(
	ctx context.Context,
	in *api.DescribeVolumesRequest,
) (*api.DescribeVolumesResult, error) {
	match, filterErr := compileFilters(in.Filters, (&volume{}).values(request.Scope{}).names()...)
	if filterErr != nil {
		return nil, filterErr
	}

	out := &api.DescribeVolumesResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := describeVolumes(tx, scope, in.VolumeIds)
		if err != nil {
			return err
		}

		all = slices.DeleteFunc(all, func(v *volume) bool { return !match.match(v.values(scope)) })
		selected, next, err := page(all, volumeID, in.NextToken, in.MaxResults, maxDescribe)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, v := range selected {
			out.Volumes = append(out.Volumes, *v.output(scope))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func volumeID(v *volume) string { return v.ID }

func describeVolumes(tx storage.ReadTx, scope request.Scope, ids []string) ([]*volume, error) {
	all, err := list[volume](tx, scope, kindVolume)
	if err != nil {
		return nil, err
	}

	return selectIDs(all, volumeID, ids, volumeNotFound)
}

func (s *Service) ModifyVolume(ctx context.Context, in *api.ModifyVolumeRequest) (*api.ModifyVolumeResult, error) {
	if in.Size == nil && in.VolumeType == nil && in.Iops == nil && in.Throughput == nil {
		return nil, invalidParameter("Specify at least one volume setting to modify")
	}

	out := &api.ModifyVolumeResult{}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := readVolume(tx, scope, common.Value(in.VolumeId))
		if err != nil {
			return err
		}

		if v.State == stateCreating || (v.Modification != nil && *v.Modification.ModificationState != stateCompleted) {
			return awserror.New("IncorrectState", "The volume is not ready to be modified")
		}

		target, err := volumeModificationTarget(v, in)
		if err != nil {
			return err
		}

		v.Modification = &api.VolumeModification{
			VolumeId: new(v.ID), ModificationState: new("modifying"), Progress: new(int64(0)),
			StartTime: new(s.now().UTC()), OriginalSize: new(v.Size), TargetSize: new(target.Size),
			OriginalVolumeType: new(v.Type), TargetVolumeType: new(target.Type),
			OriginalIops: new(v.IOPS), TargetIops: new(target.IOPS),
			OriginalThroughput: new(v.Throughput), TargetThroughput: new(target.Throughput),
		}

		out.VolumeModification = v.Modification
		return writePendingStorage(tx, scope, kindVolume, v.ID, v)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func volumeModificationTarget(v *volume, in *api.ModifyVolumeRequest) (volumeConfig, error) {
	typ, size := *common.OrElse(in.VolumeType, &v.Type), common.OrElse(in.Size, &v.Size)
	if *size < v.Size {
		return volumeConfig{}, invalidParameter("Volume size cannot decrease")
	}

	iops, throughput := in.Iops, in.Throughput
	if typ == v.Type {
		if iops == nil && (typ == volumeGP3 || typ == volumeIO1 || typ == volumeIO2) {
			iops = new(v.IOPS)
		}

		if throughput == nil && typ == volumeGP3 {
			throughput = new(v.Throughput)
		}
	}

	return resolveVolumeConfig(typ, size, iops, throughput)
}

func (s *Service) DescribeVolumesModifications(
	ctx context.Context, in *api.DescribeVolumesModificationsRequest,
) (*api.DescribeVolumesModificationsResult, error) {
	match, filterErr := compileFilters(
		in.Filters,
		filterVolumeID,
		"modification-state",
		"target-size",
		"target-volume-type",
	)
	if filterErr != nil {
		return nil, filterErr
	}

	out := &api.DescribeVolumesModificationsResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := describeVolumes(tx, scope, in.VolumeIds)
		if err != nil {
			return err
		}

		all = slices.DeleteFunc(all, func(v *volume) bool {
			m := v.Modification
			return m == nil || !match.match(values{
				filterVolumeID: {v.ID}, "modification-state": {*m.ModificationState},
				"target-size": {strconv.FormatInt(*m.TargetSize, 10)}, "target-volume-type": {*m.TargetVolumeType},
			})
		})
		selected, next, err := page(all, volumeID, in.NextToken, in.MaxResults, maxDescribe)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, v := range selected {
			out.VolumesModifications = append(out.VolumesModifications, *v.Modification)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeVolumeStatus(
	ctx context.Context, in *api.DescribeVolumeStatusRequest,
) (*api.DescribeVolumeStatusResult, error) {
	match, filterErr := compileFilters(
		in.Filters,
		filterVolumeID,
		filterZone,
		"volume-status.status",
		"volume-status.details-name",
		"volume-status.details-status",
	)
	if filterErr != nil {
		return nil, filterErr
	}

	out := &api.DescribeVolumeStatusResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := describeVolumes(tx, scope, in.VolumeIds)
		if err != nil {
			return err
		}

		all = slices.DeleteFunc(all, func(v *volume) bool {
			status, io := v.health()
			return !match.match(values{
				filterVolumeID: {v.ID}, filterZone: {v.Zone}, "volume-status.status": {status},
				"volume-status.details-name": {"io-enabled"}, "volume-status.details-status": {io},
			})
		})
		selected, next, err := page(all, volumeID, in.NextToken, in.MaxResults, maxDescribe)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, v := range selected {
			status, io := v.health()
			out.VolumeStatuses = append(out.VolumeStatuses, api.VolumeStatusItem{
				VolumeId: new(v.ID), AvailabilityZone: new(v.Zone),
				AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(v.Zone, scope.Region))),
				VolumeStatus: &api.VolumeStatusInfo{
					Status:  new(status),
					Details: []api.VolumeStatusDetails{{Name: new("io-enabled"), Status: new(io)}},
				},
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (v *volume) health() (status, io string) {
	if v.State == stateCreating {
		return "insufficient-data", "not-applicable"
	}

	return "ok", stateEnabled
}

func (s *Service) EnableEbsEncryptionByDefault(
	ctx context.Context, in *api.EnableEbsEncryptionByDefaultRequest,
) (*api.EnableEbsEncryptionByDefaultResult, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		return storage.Write(tx, scope.Key("ec2", "settings", "ebs-encryption"), true)
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableEbsEncryptionByDefaultResult{EbsEncryptionByDefault: new(true)}, nil
}

func (s *Service) GetEbsEncryptionByDefault(
	ctx context.Context, in *api.GetEbsEncryptionByDefaultRequest,
) (*api.GetEbsEncryptionByDefaultResult, error) {
	out := &api.GetEbsEncryptionByDefaultResult{EbsEncryptionByDefault: new(false)}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		enabled, _, err := storage.Read[bool](tx, scope.Key("ec2", "settings", "ebs-encryption"))
		out.EbsEncryptionByDefault = new(common.Value(enabled))
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func launchVolumeMappings(base, overrides []api.BlockDeviceMapping, root string) ([]api.BlockDeviceMapping, error) {
	mappings := slices.Clone(base)
	seen := make(map[string]bool, len(overrides))
	for _, mapping := range overrides {
		device := common.Value(mapping.DeviceName)
		if !deviceNamePattern.MatchString(device) || seen[device] {
			return nil, invalidParameter("Invalid or duplicate EBS device name: " + device)
		}

		seen[device] = true
		index := slices.IndexFunc(mappings, func(m api.BlockDeviceMapping) bool { return *m.DeviceName == device })
		if mapping.NoDevice != nil {
			if mapping.Ebs != nil || device == root || *mapping.NoDevice != "" {
				return nil, invalidParameter("NoDevice must be empty and can only suppress a non-root device")
			}

			if index >= 0 {
				mappings = slices.Delete(mappings, index, index+1)
			}

			continue
		}

		if mapping.Ebs == nil {
			return nil, invalidParameter("An EBS block device is required for " + device)
		}

		if index < 0 {
			mappings = append(mappings, mapping)
			continue
		}

		b, override := mappings[index].Ebs, mapping.Ebs
		merged := *b
		merged.DeleteOnTermination = cmp.Or(override.DeleteOnTermination, b.DeleteOnTermination)
		merged.SnapshotId = cmp.Or(override.SnapshotId, b.SnapshotId)
		merged.VolumeSize = cmp.Or(override.VolumeSize, b.VolumeSize)
		if override.SnapshotId != nil && common.Value(override.SnapshotId) != common.Value(b.SnapshotId) {
			merged.VolumeSize = override.VolumeSize
			if *override.SnapshotId == "" {
				return nil, invalidParameter("SnapshotId cannot be empty")
			}
		}

		merged.VolumeType = cmp.Or(override.VolumeType, b.VolumeType)
		merged.Encrypted = cmp.Or(override.Encrypted, b.Encrypted)
		merged.Iops, merged.Throughput = ebsPerformance(b, override)
		mappings[index].Ebs = &merged
	}

	return mappings, nil
}

func ebsPerformance(base, override *api.EbsBlockDevice) (iops, throughput *int64) {
	if override.VolumeType != nil && common.Value(override.VolumeType) != common.Value(base.VolumeType) {
		return override.Iops, override.Throughput
	}

	return cmp.Or(override.Iops, base.Iops), cmp.Or(override.Throughput, base.Throughput)
}

func launchVolumes(
	tx storage.WriteTx, scope request.Scope, i *instance, mappings []api.BlockDeviceMapping,
	fromImage, overrides []api.BlockDeviceMapping, tags map[string]string,
) error {
	for _, m := range mappings {
		trusted := slices.ContainsFunc(fromImage, func(b api.BlockDeviceMapping) bool {
			return *b.DeviceName == *m.DeviceName && common.Value(b.Ebs.SnapshotId) == common.Value(m.Ebs.SnapshotId)
		}) && !slices.ContainsFunc(overrides, func(b api.BlockDeviceMapping) bool {
			return *b.DeviceName == *m.DeviceName && b.Ebs != nil && b.Ebs.SnapshotId != nil
		})
		v, err := newVolume(tx, scope, i.LaunchTime, i.Zone, m.Ebs, tags, trusted)
		if err != nil {
			return err
		}

		if *m.DeviceName == i.RootDevice && (v.Type == volumeST1 || v.Type == volumeSC1) {
			return invalidParameter("HDD volume types are not supported for the root device")
		}

		a := volumeAttachment{
			VolumeID: v.ID, InstanceID: i.ID, Device: *m.DeviceName, Since: i.LaunchTime,
			DeleteOnTermination: *common.OrElse(m.Ebs.DeleteOnTermination, new(*m.DeviceName == i.RootDevice)),
		}

		v.State, v.Attachment = ec2InUse, new(a)
		i.Volumes = append(i.Volumes, a)
		if writeErr := storage.Write(tx, scope.Key("ec2", kindVolume, v.ID), v); writeErr != nil {
			return writeErr
		}
	}

	return nil
}
