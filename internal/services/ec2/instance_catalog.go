package ec2

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	creditStandard     = "standard"
	filterInstanceType = "instance-type"
	filterLocation     = "location"
)

func knownInstanceTypes() []string {
	var names []string
	for family := range families {
		for _, size := range sizes {
			names = append(names, family+"."+size)
		}
	}

	slices.Sort(names)
	return names
}

func creditMode(name string) string {
	family, _, _ := strings.Cut(name, ".")
	switch family {
	case "t2":
		return creditStandard
	case "t3", "t3a", vmT4g:
		return "unlimited"
	default:
		return ""
	}
}

// instanceTypeSize uses the VM sizing where available and the records
// catalogue's family ratios for its remaining synthetic size combinations.
func instanceTypeSize(name string) (cpus, memory int64) {
	if vmCPUs, vmMemory, err := VMSize(name); err == nil {
		return int64(vmCPUs), int64(vmMemory)
	}

	family, size, _ := strings.Cut(name, ".")
	if name == defaultInstanceType {
		return 1, 1740
	}

	if index := slices.Index(sizes[:4], size); index >= 0 {
		cpus = 2
		if family == "t2" && size != sizeMedium {
			cpus = 1
		}

		return cpus, 512 << index
	}

	cpus = int64(2 * instanceSizeUnits[size])
	ratio := int64(4096)
	switch family[0] {
	case 'c':
		ratio = 2048
	case 'r':
		ratio = 8192
	}

	return cpus, cpus * ratio
}

func typeOutput(name string) *api.InstanceTypeInfo {
	family, _, _ := strings.Cut(name, ".")
	cpus, memory := instanceTypeSize(name)
	return &api.InstanceTypeInfo{
		InstanceType: new(name), SupportedUsageClasses: []string{"on-demand"},
		SupportedRootDeviceTypes: []string{"ebs"}, SupportedVirtualizationTypes: []string{virtualizationHVM},
		BareMetal: new(false), InstanceStorageSupported: new(false),
		BurstablePerformanceSupported: new(creditMode(name) != ""),
		ProcessorInfo:                 &api.ProcessorInfo{SupportedArchitectures: []string{families[family]}},
		VCpuInfo: &api.VCpuInfo{
			DefaultVCpus: new(cpus),
		}, MemoryInfo: &api.MemoryInfo{SizeInMiB: new(memory)},
	}
}

func typeValues(out *api.InstanceTypeInfo) values {
	return values{
		filterInstanceType: {
			*out.InstanceType,
		},
		"processor-info.supported-architecture": out.ProcessorInfo.SupportedArchitectures,
		"vcpu-info.default-vcpus":               {strconv.FormatInt(*out.VCpuInfo.DefaultVCpus, 10)},
		"memory-info.size-in-mib":               {strconv.FormatInt(*out.MemoryInfo.SizeInMiB, 10)},
		"supported-usage-class":                 out.SupportedUsageClasses,
		"supported-root-device-type":            out.SupportedRootDeviceTypes,
		"supported-virtualization-type":         out.SupportedVirtualizationTypes,
		"bare-metal":                            boolValue(*out.BareMetal),
		"instance-storage-supported":            boolValue(*out.InstanceStorageSupported),
		"burstable-performance-supported":       boolValue(*out.BurstablePerformanceSupported),
	}
}

func (s *Service) DescribeInstanceTypes(
	ctx context.Context, in *api.DescribeInstanceTypesRequest,
) (*api.DescribeInstanceTypesResult, error) {
	if _, err := caller(ctx, in.DryRun); err != nil {
		return nil, err
	}

	match, filterErr := compileFilters(in.Filters, typeValues(typeOutput(defaultInstanceType)).names()...)
	if filterErr != nil {
		return nil, filterErr
	}

	for _, name := range in.InstanceTypes {
		if _, err := instanceType(name); err != nil {
			return nil, err
		}
	}

	var found []*api.InstanceTypeInfo
	for _, name := range knownInstanceTypes() {
		if len(in.InstanceTypes) > 0 && !slices.Contains(in.InstanceTypes, name) {
			continue
		}

		item := typeOutput(name)
		if match.match(typeValues(item)) {
			found = append(found, item)
		}
	}

	selected, next, err := page(found, func(info *api.InstanceTypeInfo) string { return *info.InstanceType },
		in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeInstanceTypesResult{NextToken: next}
	for _, item := range selected {
		out.InstanceTypes = append(out.InstanceTypes, *item)
	}

	return out, nil
}

func (s *Service) DescribeInstanceTypeOfferings(
	ctx context.Context, in *api.DescribeInstanceTypeOfferingsRequest,
) (*api.DescribeInstanceTypeOfferingsResult, error) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	kind := *common.OrElse(in.LocationType, new("region"))
	var locations []string
	switch kind {
	case "region":
		locations = []string{m.Region}
	case "availability-zone":
		locations = zoneNames(m.Region)
	case filterZoneID:
		for _, suffix := range zoneSuffixes {
			locations = append(locations, zoneID(m.Region, suffix))
		}
	default:
		return nil, invalidParameter("Unsupported LocationType: " + kind)
	}

	match, err := compileFilters(in.Filters, filterInstanceType, filterLocation)
	if err != nil {
		return nil, err
	}

	slices.Sort(locations)
	var found []*api.InstanceTypeOffering
	for _, name := range knownInstanceTypes() {
		for _, loc := range locations {
			if match.match(values{filterInstanceType: {name}, filterLocation: {loc}}) {
				found = append(found, &api.InstanceTypeOffering{
					InstanceType: new(name), LocationType: new(kind), Location: new(loc),
				})
			}
		}
	}

	selected, next, err := page(found, func(item *api.InstanceTypeOffering) string {
		return *item.InstanceType + "/" + *item.Location
	}, in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeInstanceTypeOfferingsResult{NextToken: next}
	for _, item := range selected {
		out.InstanceTypeOfferings = append(out.InstanceTypeOfferings, *item)
	}

	return out, nil
}

func (s *Service) DescribeInstanceCreditSpecifications(
	ctx context.Context, in *api.DescribeInstanceCreditSpecificationsRequest,
) (*api.DescribeInstanceCreditSpecificationsResult, error) {
	match, filterErr := compileFilters(in.Filters, filterInstanceID, "cpu-credits")
	if filterErr != nil {
		return nil, filterErr
	}

	out := &api.DescribeInstanceCreditSpecificationsResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := listInstances(tx, scope, s.now())
		if err != nil {
			return err
		}

		all, err = selectInstances(all, in.InstanceIds, in.MaxResults)
		if err != nil {
			return err
		}

		all = slices.DeleteFunc(all, func(i *instance) bool {
			mode := creditMode(i.Type)
			return !i.active() || mode == "" || !match.match(values{filterInstanceID: {i.ID}, "cpu-credits": {mode}})
		})
		selected, next, err := page(
			all,
			func(i *instance) string { return i.ID },
			in.NextToken,
			in.MaxResults,
			maxDescribe,
		)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, i := range selected {
			out.InstanceCreditSpecifications = append(out.InstanceCreditSpecifications, api.InstanceCreditSpecification{
				InstanceId: new(i.ID), CpuCredits: new(creditMode(i.Type)),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeAccountAttributes(
	ctx context.Context, in *api.DescribeAccountAttributesRequest,
) (*api.DescribeAccountAttributesResult, error) {
	out := &api.DescribeAccountAttributesResult{}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		vpcs, err := list[vpc](tx, scope, kindVPC)
		if err != nil {
			return err
		}

		defaultID := "none"
		for _, v := range vpcs {
			if v.IsDefault {
				defaultID = v.ID
				break
			}
		}

		attributes := map[string]string{
			"default-vpc": defaultID, "supported-platforms": platformVPC,
			"max-instances": strconv.Itoa(maxInstances), "vpc-max-elastic-ips": strconv.Itoa(maxAddresses),
		}

		for _, name := range in.AttributeNames {
			if _, ok := attributes[name]; !ok {
				return invalidParameter("Unsupported account attribute: " + name)
			}
		}

		for _, name := range slices.Sorted(maps.Keys(attributes)) {
			if len(in.AttributeNames) == 0 || slices.Contains(in.AttributeNames, name) {
				out.AccountAttributes = append(out.AccountAttributes, api.AccountAttribute{
					AttributeName: new(name), AttributeValues: []api.AccountAttributeValue{
						{AttributeValue: new(attributes[name])},
					},
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
