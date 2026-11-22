package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	amazonOwner    = "137112412989"
	amazonAlias    = "amazon"
	volumeGP3      = "gp3"
	canonicalOwner = "099720109477"
	archX86        = "x86_64"
	archARM        = "arm64"
	rootVolumeSize = 8
	rootXVDA       = "/dev/xvda"
	catalogDate    = "2025-01-15T00:00:00.000Z"
)

// image is a public AMI of the fixed catalog every Region has. Its ID is
// derived from the Region and name, so it is the same on every run.
type image struct {
	Name         string
	Description  string
	Architecture string
	Owner        string
	OwnerAlias   string
	RootDevice   string
	VolumeType   string
	BootMode     string
	Created      string
	// Parameters are the Systems Manager public parameters that name it.
	Parameters []string
}

var images = []image{
	{
		Name:        "al2023-ami-2023.6.20250115.0-kernel-6.1-x86_64",
		Description: "Amazon Linux 2023 AMI 2023.6.20250115.0 x86_64 HVM kernel-6.1", Architecture: archX86,
		Owner: amazonOwner, OwnerAlias: amazonAlias, RootDevice: rootXVDA, VolumeType: volumeGP3,
		BootMode: "uefi-preferred", Created: catalogDate,
		Parameters: []string{
			"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64",
			"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.1-x86_64",
		},
	},
	{
		Name:        "al2023-ami-2023.6.20250115.0-kernel-6.1-arm64",
		Description: "Amazon Linux 2023 AMI 2023.6.20250115.0 arm64 HVM kernel-6.1", Architecture: archARM,
		Owner: amazonOwner, OwnerAlias: amazonAlias, RootDevice: rootXVDA, VolumeType: volumeGP3, BootMode: "uefi",
		Created: catalogDate,
		Parameters: []string{
			"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64",
			"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.1-arm64",
		},
	},
	{
		Name:        "amzn2-ami-kernel-5.10-hvm-2.0.20250115.0-x86_64-gp2",
		Description: "Amazon Linux 2 Kernel 5.10 AMI 2.0.20250115.0 x86_64 HVM gp2", Architecture: archX86,
		Owner: amazonOwner, OwnerAlias: amazonAlias, RootDevice: rootXVDA, VolumeType: "gp2",
		BootMode: "legacy-bios", Created: catalogDate,
		Parameters: []string{
			"/aws/service/ami-amazon-linux-latest/amzn2-ami-kernel-5.10-hvm-x86_64-gp2",
			"/aws/service/ami-amazon-linux-latest/amzn2-ami-hvm-x86_64-gp2",
		},
	},
	{
		Name:        "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20250115",
		Description: "Canonical, Ubuntu, 24.04, amd64 noble image", Architecture: archX86, Owner: canonicalOwner,
		RootDevice: "/dev/sda1", VolumeType: volumeGP3, BootMode: "uefi-preferred", Created: catalogDate,
		Parameters: []string{"/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"},
	},
	{
		Name:        "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-20250115",
		Description: "Canonical, Ubuntu, 24.04, arm64 noble image", Architecture: archARM, Owner: canonicalOwner,
		RootDevice: "/dev/sda1", VolumeType: volumeGP3, BootMode: "uefi", Created: catalogDate,
		Parameters: []string{"/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id"},
	},
}

// derivedID returns a stable resource ID for a catalog entry in region.
func derivedID(prefix, region, name string) string {
	sum := sha256.Sum256([]byte(region + "/" + name))
	return prefix + "-" + hex.EncodeToString(sum[:])[:17]
}

func imageID(region string, img *image) string { return derivedID("ami", region, img.Name) }

// findImage returns the catalog image with id in region.
func findImage(region, id string) (*image, error) {
	if !strings.HasPrefix(id, "ami-") {
		return nil, awserror.New("InvalidAMIID.Malformed", fmt.Sprintf(`Invalid id: %q (expecting "ami-...")`, id))
	}

	for i := range images {
		if imageID(region, &images[i]) == id {
			return &images[i], nil
		}
	}

	return nil, missingImage(id)
}

// PublicParameters returns the Systems Manager public parameters that name
// the catalog's images in region, by parameter name.
func PublicParameters(region string) map[string]string {
	out := map[string]string{}
	for i := range images {
		for _, name := range images[i].Parameters {
			out[name] = imageID(region, &images[i])
		}
	}

	return out
}

func imageOutput(region string, img *image) api.Image {
	out := api.Image{
		ImageId:        new(imageID(region, img)),
		Name:           new(img.Name),
		Description:    new(img.Description),
		ImageLocation:  new(img.Owner + "/" + img.Name),
		State:          new(stateAvailable),
		OwnerId:        new(img.Owner),
		CreationDate:   new(img.Created),
		Public:         new(true),
		Architecture:   new(img.Architecture),
		ImageType:      new("machine"),
		RootDeviceName: new(img.RootDevice),
		RootDeviceType: new("ebs"),
		VirtualizationType: new(
			virtualizationHVM,
		),
		Hypervisor:      new("xen"),
		EnaSupport:      new(true),
		BootMode:        new(img.BootMode),
		PlatformDetails: new("Linux/UNIX"),
		UsageOperation:  new("RunInstances"),
		BlockDeviceMappings: []api.BlockDeviceMapping{{
			DeviceName: new(img.RootDevice),
			Ebs: &api.EbsBlockDevice{
				SnapshotId: new(derivedID("snap", region, img.Name)), VolumeSize: new(int64(rootVolumeSize)),
				VolumeType: new(img.VolumeType), DeleteOnTermination: new(true), Encrypted: new(false),
			},
		}},
	}
	if img.OwnerAlias != "" {
		out.ImageOwnerAlias, out.ImageLocation = new(img.OwnerAlias), new(img.OwnerAlias+"/"+img.Name)
	}

	return out
}

func imageValues(out *api.Image, tags map[string]string) values {
	vals := values{
		filterImageID:      {common.Value(out.ImageId)},
		"name":             {common.Value(out.Name)},
		filterDescription:  {common.Value(out.Description)},
		"architecture":     {common.Value(out.Architecture)},
		filterOwnerID:      {common.Value(out.OwnerId)},
		"owner-alias":      {common.Value(out.ImageOwnerAlias)},
		filterState:        {common.Value(out.State)},
		"is-public":        boolValue(common.Value(out.Public)),
		"image-type":       {common.Value(out.ImageType)},
		"root-device-name": {common.Value(out.RootDeviceName)},
		"root-device-type": {
			common.Value(out.RootDeviceType),
		},
		"virtualization-type":                        {common.Value(out.VirtualizationType)},
		"hypervisor":                                 {common.Value(out.Hypervisor)},
		"ena-support":                                boolValue(common.Value(out.EnaSupport)),
		"platform-details":                           {common.Value(out.PlatformDetails)},
		"usage-operation":                            {common.Value(out.UsageOperation)},
		"manifest-location":                          {common.Value(out.ImageLocation)},
		"creation-date":                              {common.Value(out.CreationDate)},
		"boot-mode":                                  {common.Value(out.BootMode)},
		"block-device-mapping.device-name":           {},
		"block-device-mapping.volume-type":           {},
		"block-device-mapping.volume-size":           {},
		"block-device-mapping.snapshot-id":           {},
		"block-device-mapping.delete-on-termination": {},
		"block-device-mapping.encrypted":             {},
	}
	for _, m := range out.BlockDeviceMappings {
		b := m.Ebs
		for name, value := range map[string]string{
			"device-name": common.Value(m.DeviceName), "volume-type": common.Value(b.VolumeType),
			"volume-size": strconv.FormatInt(common.Value(b.VolumeSize), 10), "snapshot-id": common.Value(b.SnapshotId),
			"delete-on-termination": strconv.FormatBool(common.Value(b.DeleteOnTermination)),
			"encrypted":             strconv.FormatBool(common.Value(b.Encrypted)),
		} {
			key := "block-device-mapping." + name
			vals[key] = append(vals[key], value)
		}
	}

	return vals.withTags(tags)
}

var accountID = regexp.MustCompile(`^\d{12}$`)

func (s *Service) DescribeImages(
	ctx context.Context,
	in *api.DescribeImagesRequest,
) (*api.DescribeImagesResult, error) {
	match, filterErr := compileFilters(in.Filters, imageValues(&api.Image{}, nil).names()...)
	if filterErr != nil {
		return nil, filterErr
	}

	if err := validatePrincipals(in.Owners, "self", amazonAlias, "aws-marketplace"); err != nil {
		return nil, err
	}

	if err := validatePrincipals(in.ExecutableUsers, "self", permissionAll); err != nil {
		return nil, err
	}

	var found []*api.Image
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := listAssets[registeredImage](tx, scope, kindImage)
		if err != nil {
			return err
		}

		visible := map[string]bool{}
		collect := func(out *api.Image, grants []string, tags map[string]string) {
			id, owner := *out.ImageId, *out.OwnerId
			if !authorized(scope.AccountID, owner, grants) {
				return
			}

			visible[id] = true
			if (len(in.ImageIds) == 0 || slices.Contains(in.ImageIds, id)) &&
				matchesOwner(scope.AccountID, owner, common.Value(out.ImageOwnerAlias), in.Owners) &&
				matchesPermission(
					scope.AccountID,
					owner,
					grants,
					in.ExecutableUsers,
				) && match.match(imageValues(out, tags)) {
				found = append(found, out)
			}
		}
		for _, img := range all {
			collect(img.output(), img.Permissions, img.Tags)
		}

		for i := range images {
			out := imageOutput(scope.Region, &images[i])
			collect(&out, []string{permissionAll}, nil)
		}

		for _, id := range in.ImageIds {
			if !visible[id] {
				return missingImage(id)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(img *api.Image) string { return *img.ImageId }
	slices.SortFunc(found, func(a, b *api.Image) int { return strings.Compare(id(a), id(b)) })
	selected, next, err := page(found, id, in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeImagesResult{Images: []api.Image{}, NextToken: next}
	for _, img := range selected {
		out.Images = append(out.Images, *img)
	}

	return out, nil
}
