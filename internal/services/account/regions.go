package account

import (
	"slices"
)

const (
	StatusEnabled          = "ENABLED"
	StatusEnabling         = "ENABLING"
	StatusDisabling        = "DISABLING"
	StatusDisabled         = "DISABLED"
	StatusEnabledByDefault = "ENABLED_BY_DEFAULT"
)

var validRegionOptStatuses = map[string]bool{
	StatusEnabled:          true,
	StatusEnabling:         true,
	StatusDisabling:        true,
	StatusDisabled:         true,
	StatusEnabledByDefault: true,
}

var allRegions = []string{
	"af-south-1", "ap-east-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-3",
	"ap-southeast-4", "ca-central-1", "eu-central-1", "eu-central-2", "eu-north-1",
	"eu-south-1", "eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"il-central-1", "me-central-1", "me-south-1", "sa-east-1", "us-east-1",
	"us-east-2", "us-west-1", "us-west-2",
}

var optInRegions = map[string]bool{
	"af-south-1":     true,
	"ap-east-1":      true,
	"ap-south-2":     true,
	"ap-southeast-3": true,
	"ap-southeast-4": true,
	"eu-central-2":   true,
	"eu-south-1":     true,
	"eu-south-2":     true,
	"il-central-1":   true,
	"me-central-1":   true,
	"me-south-1":     true,
}

func isKnownRegion(name string) bool {
	return slices.Contains(allRegions, name)
}

func defaultRegionStatus(name string) string {
	if optInRegions[name] {
		return StatusDisabled
	}
	return StatusEnabledByDefault
}
