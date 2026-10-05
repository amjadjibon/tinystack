package sfn

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	roleARN     = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/.+$`)
	logGroupARN = regexp.MustCompile(`^arn:aws[a-z-]*:logs:[a-z0-9-]+:\d{12}:log-group:[\w\-./#]{1,512}:\*$`)
	logLevels   = []string{"ALL", "ERROR", "FATAL", "OFF"}
)

func invalidDefinition(problems []string) error {
	return awserror.New("InvalidDefinition", "Invalid State Machine Definition: '"+strings.Join(problems, ", ")+"'")
}

func checkLogging(l *api.LoggingConfiguration) error {
	if l == nil {
		return nil
	}

	if l.Level != nil && !slices.Contains(logLevels, *l.Level) {
		return awserror.New("InvalidLoggingConfiguration", "Invalid Logging Configuration: level must be one of "+
			strings.Join(logLevels, ", "))
	}

	if len(l.Destinations) > 1 {
		return awserror.New("InvalidLoggingConfiguration", "Invalid Logging Configuration: at most one destination")
	}

	for _, d := range l.Destinations {
		if d.CloudWatchLogsLogGroup == nil ||
			!logGroupARN.MatchString(common.Value(d.CloudWatchLogsLogGroup.LogGroupArn)) {
			return awserror.New("InvalidLoggingConfiguration", "Invalid Logging Configuration: Log Destination not "+
				"found or not a CloudWatch Logs log group ARN ending in ':*'.")
		}
	}

	if common.Value(l.Level) != "" && common.Value(l.Level) != "OFF" && len(l.Destinations) == 0 {
		return awserror.New("InvalidLoggingConfiguration",
			"Invalid Logging Configuration: Must specify exactly one Log Destination.")
	}

	return nil
}

func checkTags(tags map[string]string) error {
	if err := tagging.Check(tags); err != nil {
		return awserror.New("TooManyTags", err.Error())
	}

	return nil
}

func loadMachine(tx storage.ReadTx, arnString *string) (request.Scope, *stateMachine, error) {
	scope, kind, name, ok := parseARN(common.Value(arnString))
	if !ok || kind != arnStateMachine || strings.Contains(name, ":") {
		return request.Scope{}, nil, invalidARN(common.Value(arnString))
	}

	sm, found, err := storage.Read[stateMachine](tx, machineKey(scope, name))
	if err != nil || !found {
		if err == nil {
			err = awserror.New(
				"StateMachineDoesNotExist",
				"State Machine Does Not Exist: '"+common.Value(arnString)+"'",
			)
		}

		return scope, nil, err
	}

	return scope, sm, nil
}

// inScope checks that an ARN is in the caller's account and region.
func inScope(m *request.Metadata, scope request.Scope, arnString *string) error {
	if scope != m.Scope {
		return invalidARN(common.Value(arnString))
	}

	return nil
}

func (s *Service) CreateStateMachine(
	ctx context.Context,
	in *api.CreateStateMachineInput,
) (*api.CreateStateMachineOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if checkErr := checkName(name); checkErr != nil {
		return nil, checkErr
	}

	if _, problems := parseDefinition(common.Value(in.Definition)); len(problems) > 0 {
		return nil, invalidDefinition(problems)
	}

	kind := common.Value(in.Type)
	switch {
	case kind == "":
		kind = typeStandard
	case kind != typeStandard && kind != typeExpress:
		return nil, awserror.Validation("type must be STANDARD or EXPRESS")
	}

	switch {
	case !roleARN.MatchString(common.Value(in.RoleArn)):
		return nil, invalidARN(common.Value(in.RoleArn))
	case in.VersionDescription != nil && !common.Value(in.Publish):
		return nil, awserror.Validation("versionDescription requires publish")
	case len(common.Value(in.VersionDescription)) > 256:
		return nil, awserror.Validation("versionDescription must be at most 256 characters")
	}

	if checkErr := checkLogging(in.LoggingConfiguration); checkErr != nil {
		return nil, checkErr
	}

	tags := tagsOf(in.Tags)
	if checkErr := checkTags(tags); checkErr != nil {
		return nil, checkErr
	}

	sm := &stateMachine{
		Name: name, Definition: *in.Definition, RoleArn: *in.RoleArn, Type: kind, Tags: tags,
		Logging: in.LoggingConfiguration, Tracing: in.TracingConfiguration, RevisionID: uuid.NewV4().String(),
		Created: common.Unix(s.now()),
	}
	if sm.Logging == nil {
		sm.Logging = &api.LoggingConfiguration{Level: new("OFF"), IncludeExecutionData: new(false)}
	}

	if sm.Tracing == nil {
		sm.Tracing = &api.TracingConfiguration{Enabled: new(false)}
	}

	out := &api.CreateStateMachineOutput{StateMachineArn: new(arn(m.Scope, arnStateMachine, name))}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := machineKey(m.Scope, name)
		existing, found, readErr := storage.Read[stateMachine](tx, key)
		if readErr != nil {
			return readErr
		}

		// Creating the same state machine again returns it.
		if found {
			if existing.Definition != sm.Definition || existing.RoleArn != sm.RoleArn || existing.Type != sm.Type {
				return awserror.New("StateMachineAlreadyExists", "State Machine Already Exists: '"+
					*out.StateMachineArn+"'")
			}

			out.CreationDate = new(existing.Created)
			return nil
		}

		out.CreationDate = new(sm.Created)
		if writeErr := storage.Write(tx, key, sm); writeErr != nil || !common.Value(in.Publish) {
			return writeErr
		}

		v, publishErr := publish(tx, m.Scope, sm, common.Value(in.VersionDescription), sm.Created)
		if publishErr == nil {
			out.StateMachineVersionArn = new(v.arn(m.Scope))
		}

		return publishErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func describeMachine(scope request.Scope, sm *stateMachine) *api.DescribeStateMachineOutput {
	return &api.DescribeStateMachineOutput{
		StateMachineArn: new(arn(scope, arnStateMachine, sm.Name)), Name: new(sm.Name), Status: new("ACTIVE"),
		Definition: new(sm.Definition), RoleArn: new(sm.RoleArn), Type: new(sm.Type),
		CreationDate: new(sm.Created), LoggingConfiguration: sm.Logging, TracingConfiguration: sm.Tracing,
		RevisionId: new(sm.RevisionID),
	}
}

func (s *Service) DescribeStateMachine(
	ctx context.Context,
	in *api.DescribeStateMachineInput,
) (*api.DescribeStateMachineOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, machine, qualifier, err := qualified(common.Value(in.StateMachineArn))
	if err == nil && qualifier != "" && !isVersion(qualifier) {
		err = invalidARN(common.Value(in.StateMachineArn))
	}

	if err == nil {
		err = inScope(&m, scope, in.StateMachineArn)
	}

	if err != nil {
		return nil, err
	}

	var out *api.DescribeStateMachineOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, sm, loadErr := loadMachine(tx, new(machineARN(scope, machine)))
		if loadErr != nil || qualifier == "" {
			if loadErr == nil {
				out = describeMachine(scope, sm)
			}

			return loadErr
		}

		n, _ := strconv.ParseInt(qualifier, 10, 64)
		v, loadErr := loadVersion(tx, scope, machine, n)
		if loadErr != nil {
			return loadErr
		}

		out = describeMachine(scope, &v.Machine)
		out.StateMachineArn = new(v.arn(scope))
		if v.Description != "" {
			out.Description = new(v.Description)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListStateMachines(
	ctx context.Context,
	in *api.ListStateMachinesInput,
) (*api.ListStateMachinesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListStateMachinesOutput{StateMachines: []api.StateMachineListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var items []api.StateMachineListItem
		for _, k := range tx.Keys(m.Prefix(service, kindMachine)) {
			sm, _, readErr := storage.Read[stateMachine](tx, k)
			if readErr != nil {
				return readErr
			}

			items = append(items, api.StateMachineListItem{
				StateMachineArn: new(arn(m.Scope, arnStateMachine, sm.Name)), Name: new(sm.Name), Type: new(sm.Type),
				CreationDate: new(sm.Created),
			})
		}

		var pageErr error
		items, out.NextToken, pageErr = page(items, func(i api.StateMachineListItem) string { return *i.Name },
			in.NextToken, in.MaxResults)
		out.StateMachines = append(out.StateMachines, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateStateMachine(
	ctx context.Context,
	in *api.UpdateStateMachineInput,
) (*api.UpdateStateMachineOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case in.Definition == nil && in.RoleArn == nil && in.LoggingConfiguration == nil && in.TracingConfiguration == nil:
		return nil, awserror.New("MissingRequiredParameter", "Either the definition, the roleArn, the "+
			"loggingConfiguration, or the tracingConfiguration must be specified")
	case in.RoleArn != nil && !roleARN.MatchString(*in.RoleArn):
		return nil, invalidARN(*in.RoleArn)
	case in.VersionDescription != nil && !common.Value(in.Publish):
		return nil, awserror.Validation("versionDescription requires publish")
	}

	if in.Definition != nil {
		if _, problems := parseDefinition(*in.Definition); len(problems) > 0 {
			return nil, invalidDefinition(problems)
		}
	}

	if checkErr := checkLogging(in.LoggingConfiguration); checkErr != nil {
		return nil, checkErr
	}

	now := common.Unix(s.now())
	var sm *stateMachine
	out := &api.UpdateStateMachineOutput{UpdateDate: &now}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, loaded, loadErr := loadMachine(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		sm = loaded
		sm.Definition, sm.RoleArn = common.Value(
			common.OrElse(in.Definition, &sm.Definition),
		), common.Value(
			common.OrElse(in.RoleArn, &sm.RoleArn),
		)
		sm.Logging, sm.Tracing = common.OrElse(
			in.LoggingConfiguration,
			sm.Logging,
		), common.OrElse(
			in.TracingConfiguration,
			sm.Tracing,
		)
		sm.RevisionID = uuid.NewV4().String()
		out.RevisionId = new(sm.RevisionID)
		if writeErr := storage.Write(tx, machineKey(scope, sm.Name), sm); writeErr != nil || !common.Value(in.Publish) {
			return writeErr
		}

		v, publishErr := publish(tx, scope, sm, common.Value(in.VersionDescription), now)
		if publishErr == nil {
			out.StateMachineVersionArn = new(v.arn(scope))
		}

		return publishErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteStateMachine deletes a state machine with its executions, stopping
// those still running.
func (s *Service) DeleteStateMachine(
	ctx context.Context,
	in *api.DeleteStateMachineInput,
) (*api.DeleteStateMachineOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var running []string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, sm, loadErr := loadMachine(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		for _, kind := range []string{kindExecution, kindEvent, kindVersion, kindAlias} {
			for _, k := range tx.Keys(scope.Prefix(service, kind) + sm.Name + "/") {
				if kind == kindExecution {
					e, _, readErr := storage.Read[execution](tx, k)
					if readErr != nil {
						return readErr
					}

					if e.Status == statusRunning {
						running = append(running, e.ARN)
					}
				}

				tx.Delete(k)
			}
		}

		tx.Delete(machineKey(scope, sm.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, e := range running {
		s.cancelRun(e, &stopError{error: "States.Aborted", cause: "The state machine was deleted."})
	}

	return &api.DeleteStateMachineOutput{}, nil
}

func (s *Service) ValidateStateMachineDefinition(
	_ context.Context,
	in *api.ValidateStateMachineDefinitionInput,
) (*api.ValidateStateMachineDefinitionOutput, error) {
	_, problems := parseDefinition(common.Value(in.Definition))
	out := &api.ValidateStateMachineDefinitionOutput{
		Result: new("OK"), Diagnostics: []api.ValidateStateMachineDefinitionDiagnostic{}, Truncated: new(false),
	}
	for _, p := range problems {
		code, message, _ := strings.Cut(p, ": ")
		location := ""
		if i := strings.LastIndex(message, " at /"); i >= 0 {
			message, location = message[:i], message[i+4:]
		}

		out.Result = new("FAIL")
		out.Diagnostics = append(out.Diagnostics, api.ValidateStateMachineDefinitionDiagnostic{
			Severity: new("ERROR"), Code: new(code), Message: new(message), Location: new(location),
		})
	}

	if limit := common.Value(in.MaxResults); limit > 0 && int64(len(out.Diagnostics)) > limit {
		out.Diagnostics, out.Truncated = out.Diagnostics[:limit], new(true)
	}

	return out, nil
}
