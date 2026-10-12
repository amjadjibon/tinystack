package codepipeline

import (
	"maps"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
)

const (
	categorySource = "Source"
	modeSuperseded = "SUPERSEDED"
	typeV1         = "V1"
	typeV2         = "V2"
	// defaultRunOrder is the run order of an action that declares none.
	defaultRunOrder = int64(1)
)

// The enums, patterns and lengths come from the pinned CodePipeline model. The generated decoder checks none of them.
var (
	executionModes = []string{"QUEUED", modeSuperseded, "PARALLEL"}
	pipelineTypes  = []string{typeV1, typeV2}
	categories     = []string{categorySource, "Build", "Deploy", "Test", "Invoke", "Approval", "Compute"}
	owners         = []string{"AWS", "ThirdParty", "Custom"}

	namePattern     = regexp.MustCompile(`^[A-Za-z0-9.@\-_]+$`)
	rolePattern     = regexp.MustCompile(`^arn:aws(-\w+)*:iam::\d{12}:role/`)
	artifactPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)
	tokenPattern    = regexp.MustCompile(`^[0-9A-Za-z_-]+$`)
	bucketPattern   = regexp.MustCompile(`^[a-zA-Z0-9\-.]+$`)
	variablePattern = regexp.MustCompile(`^[A-Za-z0-9@\-_]+$`)
)

func checkText(what string, v *string, minLen, maxLen int, pattern *regexp.Regexp) error {
	if n := utf8.RuneCountInString(*v); n < minLen || n > maxLen {
		return awserror.Validation("%s must be %d to %d characters long", what, minLen, maxLen)
	}

	if pattern != nil && !pattern.MatchString(*v) {
		return awserror.Validation("%s must match the pattern %s", what, pattern)
	}

	return nil
}

func requiredText(what string, v *string, minLen, maxLen int, pattern *regexp.Regexp) error {
	if v == nil {
		return awserror.Validation("%s is required", what)
	}

	return checkText(what, v, minLen, maxLen, pattern)
}

func optionalText(what string, v *string, minLen, maxLen int, pattern *regexp.Regexp) error {
	if v == nil {
		return nil
	}

	return checkText(what, v, minLen, maxLen, pattern)
}

func requiredEnum(what string, v *string, allowed []string) error {
	if v == nil {
		return awserror.Validation("%s is required", what)
	}

	if !slices.Contains(allowed, *v) {
		return awserror.Validation("%s must be one of %v", what, allowed)
	}

	return nil
}

// normalize validates the whole declaration and fills in the values AWS reports for omitted ones: each action's
// runOrder, the executionMode and the pipelineType. p must belong to the caller of normalize, which keeps no version.
func normalize(p *api.PipelineDeclaration, region string) error {
	if err := checkSettings(p); err != nil {
		return err
	}

	if err := checkStores(p, region); err != nil {
		return err
	}

	return checkStages(p, region)
}

func checkSettings(p *api.PipelineDeclaration) error {
	if err := requiredText("pipeline.name", p.Name, 1, 100, namePattern); err != nil {
		return err
	}

	if err := requiredText("pipeline.roleArn", p.RoleArn, 0, 1024, rolePattern); err != nil {
		return err
	}

	if p.Version != nil && *p.Version < 1 {
		return awserror.Validation("pipeline.version must be at least 1")
	}

	if p.ExecutionMode == nil {
		p.ExecutionMode = new(modeSuperseded)
	} else if err := requiredEnum("pipeline.executionMode", p.ExecutionMode, executionModes); err != nil {
		return err
	}

	if p.PipelineType != nil {
		if err := requiredEnum("pipeline.pipelineType", p.PipelineType, pipelineTypes); err != nil {
			return err
		}
	}

	if err := checkVariables(p.Variables); err != nil {
		return err
	}

	// Pipeline variables and the QUEUED and PARALLEL execution modes are V2 features, and a declaration that uses
	// one without naming a type becomes a V2 pipeline.
	v2Features := len(p.Variables) > 0 || *p.ExecutionMode != modeSuperseded
	switch {
	case p.PipelineType == nil && v2Features:
		p.PipelineType = new(typeV2)
	case p.PipelineType == nil:
		p.PipelineType = new(typeV1)
	case *p.PipelineType == typeV1 && v2Features:
		return invalidStructure("Pipeline variables and the %s execution mode require a V2 pipeline", *p.ExecutionMode)
	}

	return nil
}

func checkVariables(variables []api.PipelineVariableDeclaration) error {
	for i := range variables {
		v := &variables[i]
		if err := requiredText("variable name", v.Name, 1, 128, variablePattern); err != nil {
			return err
		}

		if err := optionalText("variable defaultValue", v.DefaultValue, 1, 1000, nil); err != nil {
			return err
		}

		if err := optionalText("variable description", v.Description, 0, 200, nil); err != nil {
			return err
		}
	}

	return nil
}

// checkStores requires exactly one of artifactStore and artifactStores, and with artifactStores a store for the
// pipeline's Region.
func checkStores(p *api.PipelineDeclaration, region string) error {
	single, multiple := p.ArtifactStore != nil, len(p.ArtifactStores) > 0
	switch {
	case single == multiple:
		return invalidStructure("Specify either artifactStore or artifactStores, and not both")
	case single:
		return checkStore("artifactStore", p.ArtifactStore)
	}

	if _, ok := p.ArtifactStores[region]; !ok {
		return invalidStructure("artifactStores needs an artifact store for the pipeline Region %s", region)
	}

	for _, r := range slices.Sorted(maps.Keys(p.ArtifactStores)) {
		if err := checkText("artifactStores Region", &r, 4, 30, nil); err != nil {
			return err
		}

		store := p.ArtifactStores[r]
		if err := checkStore("artifactStores."+r, &store); err != nil {
			return err
		}
	}

	return nil
}

func checkStore(what string, s *api.ArtifactStore) error {
	if err := requiredEnum(what+".type", s.Type, []string{"S3"}); err != nil {
		return err
	}

	if err := requiredText(what+".location", s.Location, 3, 63, bucketPattern); err != nil {
		return err
	}

	if s.EncryptionKey == nil {
		return nil
	}

	if err := requiredText(what+".encryptionKey.id", s.EncryptionKey.Id, 1, 400, nil); err != nil {
		return err
	}

	return requiredEnum(what+".encryptionKey.type", s.EncryptionKey.Type, []string{"KMS"})
}

// checkStages applies the documented structure rules: at least two stages; unique stage names; at least one action
// per stage; unique action names within a stage; Source actions only, and only, in the first stage; and artifacts
// that each earlier action produces once and later actions consume.
func checkStages(p *api.PipelineDeclaration, region string) error {
	if len(p.Stages) < 2 {
		return invalidStructure("A pipeline must contain at least two stages")
	}

	hasStore := func(r string) bool {
		if len(p.ArtifactStores) > 0 {
			_, ok := p.ArtifactStores[r]
			return ok
		}

		return r == region
	}
	names := map[string]bool{}
	for i := range p.Stages {
		stage := &p.Stages[i]
		if err := requiredText("stage name", stage.Name, 1, 100, namePattern); err != nil {
			return err
		}

		if names[*stage.Name] {
			return invalidStage("Stage name %s is used more than once", *stage.Name)
		}

		names[*stage.Name] = true
		if err := checkStage(stage, i == 0, hasStore); err != nil {
			return err
		}
	}

	return checkArtifacts(p.Stages)
}

func checkStage(stage *api.StageDeclaration, first bool, hasStore func(string) bool) error {
	if len(stage.Actions) == 0 {
		return invalidStage("Stage %s needs at least one action", *stage.Name)
	}

	names := map[string]bool{}
	for i := range stage.Actions {
		a := &stage.Actions[i]
		if err := checkAction(a, hasStore); err != nil {
			return err
		}

		if names[*a.Name] {
			return invalidAction("Action name %s is used more than once in stage %s", *a.Name, *stage.Name)
		}

		names[*a.Name] = true
		source := *a.ActionTypeId.Category == categorySource
		if first && !source {
			return invalidStage("The first stage %s can only contain Source actions", *stage.Name)
		}

		if !first && source {
			return invalidStage("Only the first stage can contain Source actions, not stage %s", *stage.Name)
		}
	}

	return nil
}

func checkAction(a *api.ActionDeclaration, hasStore func(string) bool) error {
	if err := requiredText("action name", a.Name, 1, 100, namePattern); err != nil {
		return err
	}

	if err := checkActionType(a.ActionTypeId); err != nil {
		return err
	}

	if a.RunOrder == nil {
		a.RunOrder = new(defaultRunOrder)
	} else if *a.RunOrder < 1 || *a.RunOrder > 999 {
		return awserror.Validation("action runOrder must be 1 to 999")
	}

	for k, v := range a.Configuration {
		if err := checkText("action configuration key", &k, 1, 50, nil); err != nil {
			return err
		}

		if err := checkText("action configuration value", &v, 1, 1000, nil); err != nil {
			return err
		}
	}

	if err := optionalText("action roleArn", a.RoleArn, 0, 1024, rolePattern); err != nil {
		return err
	}

	if err := optionalText("action namespace", a.Namespace, 1, 100, variablePattern); err != nil {
		return err
	}

	if err := optionalText("action region", a.Region, 4, 30, nil); err != nil {
		return err
	}

	// An action in another Region needs an artifact store there, which only artifactStores can hold.
	if a.Region != nil && !hasStore(*a.Region) {
		return invalidAction("Action %s runs in %s, which has no artifact store", *a.Name, *a.Region)
	}

	for _, in := range a.InputArtifacts {
		if err := requiredText("input artifact name", in.Name, 1, 100, artifactPattern); err != nil {
			return err
		}
	}

	for _, out := range a.OutputArtifacts {
		if err := requiredText("output artifact name", out.Name, 1, 100, artifactPattern); err != nil {
			return err
		}
	}

	return nil
}

func checkActionType(t *api.ActionTypeId) error {
	if t == nil {
		return awserror.Validation("action actionTypeId is required")
	}

	if err := requiredEnum("action category", t.Category, categories); err != nil {
		return err
	}

	if err := requiredEnum("action owner", t.Owner, owners); err != nil {
		return err
	}

	if err := requiredText("action provider", t.Provider, 1, 35, tokenPattern); err != nil {
		return err
	}

	return requiredText("action version", t.Version, 1, 9, tokenPattern)
}

// checkArtifacts requires output artifact names to be unique in the pipeline, and every input artifact to be the
// output of an action in an earlier stage or of one that runs earlier in the same stage. Actions of one runOrder run
// in parallel, so they cannot pass artifacts to each other.
func checkArtifacts(stages []api.StageDeclaration) error {
	earlier := map[string]bool{}
	for i := range stages {
		stage := &stages[i]
		outputs := map[string]int64{}
		for j := range stage.Actions {
			a := &stage.Actions[j]
			for _, out := range a.OutputArtifacts {
				if _, dup := outputs[*out.Name]; dup || earlier[*out.Name] {
					return invalidAction("Output artifact %s is declared more than once", *out.Name)
				}

				outputs[*out.Name] = *a.RunOrder
			}
		}

		for j := range stage.Actions {
			a := &stage.Actions[j]
			for _, in := range a.InputArtifacts {
				if runOrder, ok := outputs[*in.Name]; !earlier[*in.Name] && (!ok || runOrder >= *a.RunOrder) {
					return invalidAction(
						"Input artifact %s of action %s is not the output of an earlier action", *in.Name, *a.Name,
					)
				}
			}
		}

		for name := range outputs {
			earlier[name] = true
		}
	}

	return nil
}
