package appconfig

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/common"
)

func validateStrategy(d *api.DeploymentStrategy) error {
	if d.DeploymentDurationInMinutes == nil || *d.DeploymentDurationInMinutes < 0 ||
		*d.DeploymentDurationInMinutes > 1440 {
		return bad("DeploymentDurationInMinutes must be between 0 and 1440")
	}

	if common.Value(d.FinalBakeTimeInMinutes) < 0 || common.Value(d.FinalBakeTimeInMinutes) > 1440 {
		return bad("FinalBakeTimeInMinutes must be between 0 and 1440")
	}

	if d.GrowthFactor == nil || *d.GrowthFactor <= 0 || *d.GrowthFactor > 100 {
		return bad("GrowthFactor must be in (0,100]")
	}

	if common.Value(d.GrowthType) != growthLinear && common.Value(d.GrowthType) != growthExponential {
		return bad("GrowthType must be LINEAR or EXPONENTIAL")
	}

	if common.Value(d.ReplicateTo) != "NONE" {
		return bad("Only ReplicateTo NONE is supported")
	}

	return nil
}

func predefinedStrategies() []*record {
	var out []*record
	for _, v := range []struct {
		name, growth   string
		duration, bake int64
		factor         float64
	}{
		{"AppConfig.AllAtOnce", growthLinear, 0, 10, 100},
		{"AppConfig.Canary10Percent20Minutes", growthExponential, 20, 10, 10},
		{"AppConfig.Linear20PercentEvery6Minutes", growthLinear, 30, 30, 20},
		{"AppConfig.Linear50PercentEvery30Seconds", growthLinear, 1, 1, 50},
	} {
		out = append(out, &record{ID: v.name, Name: v.name, Kind: kindStrategy, Strategy: &api.DeploymentStrategy{
			Id: new(v.name), Name: new(v.name), GrowthType: new(v.growth), DeploymentDurationInMinutes: new(v.duration),
			FinalBakeTimeInMinutes: new(v.bake), GrowthFactor: new(v.factor), ReplicateTo: new("NONE"),
		}})
	}

	return out
}

func predefined(id string) *record {
	for _, r := range predefinedStrategies() {
		if r.ID == id {
			return r
		}
	}

	return nil
}
