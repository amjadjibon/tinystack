package ssm

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusPending    = "Pending"
	statusInProgress = "InProgress"
	statusSuccess    = "Success"
	statusCancelled  = "Cancelled"
	statusFailed     = "Failed"
	statusTimedOut   = "TimedOut"

	maxCommandInstances = 50
	maxTargets          = 5
	maxTargetValues     = 50
	maxCommandPage      = 50
	defaultTimeout      = 600
	minTimeout          = 30
	maxTimeout          = 2592000
	maxComment          = 100
	// settleAfter is how long a command stays Pending before its
	// invocations finish, so that Cancel has a moment to act.
	settleAfter = time.Second

	commandIndexPrefix = "aws/ssm/index/cmd/"
	timestampLayout    = "2006-01-02T15:04:05.000Z"
)

// Instance is an instance Run Command can target.
type Instance struct {
	Tags map[string]string
	ID   string
}

// Instances lists the running instances of an account and Region.
type Instances func(ctx context.Context, scope request.Scope) ([]Instance, error)

// UseInstances lets SendCommand check that instances exist and find them by
// tag. Without it, SendCommand accepts any instance ID and no tag targets.
// Call it before serving requests.
func (s *Service) UseInstances(fn Instances) { s.instances = fn }

type commandTarget struct {
	Key    string
	Values []string
}

// command is a Run Command request. No command runs: each invocation
// finishes as Success with empty output, shortly after the command is sent
// unless it is cancelled first.
type command struct {
	Parameters  map[string][]string `json:",omitempty"`
	CloudWatch  *api.CloudWatchOutputConfig
	ID          string
	Document    string
	DocVersion  string
	Comment     string `json:",omitempty"`
	Status      string
	Details     string
	S3Region    string `json:",omitempty"`
	S3Bucket    string `json:",omitempty"`
	S3Prefix    string `json:",omitempty"`
	Concurrency string
	MaxErrors   string
	ServiceRole string `json:",omitempty"`
	// InstanceIDs and Targets are the request's, as SendCommand returns them.
	InstanceIDs []string        `json:",omitempty"`
	Targets     []commandTarget `json:",omitempty"`
	// Plugins are the plugins of the document version the command runs.
	Plugins     []string `json:",omitempty"`
	RequestedAt int64    // Unix milliseconds
	ExpiresAt   int64
	Timeout     int64
	TargetCount int64
	Completed   int64
	Errors      int64
}

// invocation is a command's run on one instance.
type invocation struct {
	CommandID  string
	InstanceID string
	Name       string `json:",omitempty"`
	Status     string
	StartedAt  int64 `json:",omitempty"`
	FinishedAt int64 `json:",omitempty"`
}

func (i *invocation) finished() bool {
	return i.Status != statusPending && i.Status != statusInProgress
}

func commandKey(scope request.Scope, id string) string { return scope.Key("ssm", "c", id) }

func invocationPrefix(scope request.Scope, id string) string { return scope.Key("ssm", "ci", id+"/") }

func invocationKey(scope request.Scope, id, instance string) string {
	return invocationPrefix(scope, id) + instance
}

func commandIndexKey(scope request.Scope, id string) string {
	return commandIndexPrefix + scope.AccountID + "/" + scope.Region + "/" + id
}

func invalidCommandID() error {
	return awserror.New("InvalidCommandId", "The command ID is not valid.")
}

func invalidInstance(id, account string) error {
	return awserror.New("InvalidInstanceId", fmt.Sprintf("Instances [[%s]] not in a valid state for account %s", id,
		account))
}

func newCommandID() string {
	h := common.RandomID(32)

	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (c *command) output() api.Command {
	out := api.Command{
		CommandId:          new(c.ID),
		DocumentName:       new(c.Document),
		DocumentVersion:    new(c.DocVersion),
		Comment:            optional(c.Comment),
		ExpiresAfter:       seconds(c.ExpiresAt),
		Parameters:         c.Parameters,
		InstanceIds:        c.InstanceIDs,
		RequestedDateTime:  seconds(c.RequestedAt),
		Status:             new(c.Status),
		StatusDetails:      new(c.Details),
		OutputS3Region:     optional(c.S3Region),
		OutputS3BucketName: optional(c.S3Bucket),
		OutputS3KeyPrefix:  optional(c.S3Prefix),
		MaxConcurrency:     new(c.Concurrency),
		MaxErrors:          new(c.MaxErrors),
		TargetCount:        new(c.TargetCount),
		CompletedCount:     new(c.Completed),
		ErrorCount:         new(c.Errors),
		DeliveryTimedOutCount: new(
			int64(0),
		),
		ServiceRole:            optional(c.ServiceRole),
		CloudWatchOutputConfig: c.CloudWatch,
		TimeoutSeconds:         new(c.Timeout),
	}
	for _, t := range c.Targets {
		out.Targets = append(out.Targets, api.Target{Key: new(t.Key), Values: t.Values})
	}

	return out
}

// settle recomputes a command's status and counts from its invocations.
func (c *command) settle(invocations []*invocation) {
	c.TargetCount = int64(len(invocations))
	c.Completed, c.Errors = 0, 0
	var running, cancelled, failed bool
	for _, i := range invocations {
		if i.finished() {
			c.Completed++
		} else if i.Status == statusInProgress {
			running = true
		}

		switch i.Status {
		case statusCancelled:
			cancelled = true
		case statusFailed, statusTimedOut:
			failed = true
			c.Errors++
		}
	}

	switch {
	case c.Completed < c.TargetCount && running:
		c.Status = statusInProgress
	case c.Completed < c.TargetCount:
		c.Status = statusPending
	case cancelled:
		c.Status = statusCancelled
	case failed:
		c.Status = statusFailed
	default:
		c.Status = statusSuccess
	}

	c.Details = c.Status
}

// matchTargets returns the IDs of the running instances every target selects.
func matchTargets(instances []Instance, targets []commandTarget) []string {
	var out []string
	for _, i := range instances {
		ok := true
		for _, t := range targets {
			switch tag, isTag := strings.CutPrefix(t.Key, "tag:"); {
			case isTag:
				got, has := i.Tags[tag]
				ok = ok && has && slices.Contains(t.Values, got)
			default:
				ok = ok && slices.Contains(t.Values, i.ID)
			}
		}

		if ok {
			out = append(out, i.ID)
		}
	}

	return out
}

// resolveTargets checks the instance IDs and targets of a SendCommand
// request, outside any transaction, and returns the instances to run on.
func (s *Service) resolveTargets(ctx context.Context, scope request.Scope, in *api.SendCommandRequest) (
	ids []string,
	targets []commandTarget,
	err error,
) {
	if len(in.InstanceIds) > 0 == (len(in.Targets) > 0) {
		return nil, nil, validation("Specify either InstanceIds or Targets.")
	}

	var known []Instance
	if s.instances != nil {
		if known, err = s.instances(ctx, scope); err != nil {
			return nil, nil, err
		}
	}

	if len(in.InstanceIds) > 0 {
		return s.checkInstanceIDs(scope, in.InstanceIds, known)
	}

	if s.instances == nil {
		return nil, nil, validation("This TinyStack has no EC2 service to resolve Targets with.")
	}

	if len(in.Targets) > maxTargets {
		return nil, nil, validation("Targets can list at most %d targets.", maxTargets)
	}

	for _, t := range in.Targets {
		key := value(t.Key, "")
		if _, isTag := strings.CutPrefix(key, "tag:"); (!isTag && key != "InstanceIds") || len(key) == len("tag:") {
			return nil, nil, validation("TinyStack supports the targets tag:<key> and InstanceIds, not %q.", key)
		}

		if len(t.Values) == 0 || len(t.Values) > maxTargetValues {
			return nil, nil, validation("A target needs 1 to %d values.", maxTargetValues)
		}

		targets = append(targets, commandTarget{Key: key, Values: t.Values})
	}

	return matchTargets(known, targets), targets, nil
}

func (s *Service) checkInstanceIDs(scope request.Scope, ids []string, known []Instance) (
	[]string,
	[]commandTarget,
	error,
) {
	if len(ids) > maxCommandInstances {
		return nil, nil, validation("InstanceIds can list at most %d instances.", maxCommandInstances)
	}

	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			return nil, nil, awserror.New("DuplicateInstanceId", "Duplicate instance ID: "+id)
		}

		if s.instances != nil && !slices.ContainsFunc(known, func(k Instance) bool { return k.ID == id }) {
			return nil, nil, invalidInstance(id, scope.AccountID)
		}
	}

	return ids, nil, nil
}

// checkCommandParameters requires the parameters to be the document's, and
// every parameter without a default to be present.
func checkCommandParameters(c *content, given map[string][]string) error {
	declared := map[string]bool{}
	for _, p := range c.Parameters {
		declared[*p.Name] = true
	}

	for name := range given {
		if !declared[name] {
			return awserror.New("InvalidParameters", "The parameter "+name+" is not defined by the document.")
		}
	}

	for _, name := range c.required() {
		if len(given[name]) == 0 {
			return awserror.New("InvalidParameters", "The parameter "+name+" is required by the document.")
		}
	}

	return nil
}

func (s *Service) SendCommand(ctx context.Context, in *api.SendCommandRequest) (*api.SendCommandResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.DocumentName == nil {
		return nil, validation("DocumentName is required.")
	}

	timeout := int64(defaultTimeout)
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}

	if timeout < minTimeout || timeout > maxTimeout {
		return nil, validation("TimeoutSeconds must be between %d and %d.", minTimeout, maxTimeout)
	}

	if len(value(in.Comment, "")) > maxComment {
		return nil, validation("Comment must be at most %d characters long.", maxComment)
	}

	ids, targets, err := s.resolveTargets(ctx, m.Scope, in)
	if err != nil {
		return nil, err
	}

	var instanceNames map[string]string
	if s.instances != nil {
		known, listErr := s.instances(ctx, m.Scope)
		if listErr != nil {
			return nil, listErr
		}

		instanceNames = map[string]string{}
		for _, i := range known {
			instanceNames[i.ID] = i.Tags["Name"]
		}
	}

	now := s.now().UnixMilli()
	c := &command{
		ID: newCommandID(), Document: "", DocVersion: value(in.DocumentVersion, defaultShare),
		Comment: value(in.Comment, ""), Parameters: in.Parameters, CloudWatch: in.CloudWatchOutputConfig,
		S3Region: value(in.OutputS3Region, ""), S3Bucket: value(in.OutputS3BucketName, ""),
		S3Prefix: value(in.OutputS3KeyPrefix, ""), Concurrency: value(in.MaxConcurrency, "50"),
		MaxErrors: value(in.MaxErrors, "0"), ServiceRole: value(in.ServiceRoleArn, ""), InstanceIDs: in.InstanceIds,
		Targets: targets, RequestedAt: now, ExpiresAt: now + timeout*1000, Timeout: timeout,
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, v, findErr := s.find(tx, m.Scope, in.DocumentName, in.DocumentVersion, nil)
		if findErr != nil {
			return findErr
		}

		if rec.Type != typeCommandDoc {
			return invalidDocument("Only Command documents can be sent.")
		}

		parsed, parseErr := parseContent(v.Content, v.Format, rec.Type)
		if parseErr != nil {
			return parseErr
		}

		if paramErr := checkCommandParameters(parsed, in.Parameters); paramErr != nil {
			return paramErr
		}

		c.Document, c.Plugins = rec.Name, parsed.Plugins

		var invocations []*invocation
		for _, id := range ids {
			invocations = append(invocations, &invocation{
				CommandID: c.ID, InstanceID: id, Name: instanceNames[id], Status: statusPending,
			})
		}

		c.settle(invocations)
		if len(invocations) == 0 {
			c.Status, c.Details = statusSuccess, statusSuccess
		}

		for _, i := range invocations {
			if writeErr := storage.Write(tx, invocationKey(m.Scope, c.ID, i.InstanceID), i); writeErr != nil {
				return writeErr
			}
		}

		if len(invocations) > 0 {
			if writeErr := storage.Write(tx, commandIndexKey(m.Scope, c.ID), now); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, commandKey(m.Scope, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	out := c.output()

	return &api.SendCommandResult{Command: &out}, nil
}

// finishCommands finishes the invocations of commands that were sent more
// than settleAfter ago and are still waiting.
func (s *Service) finishCommands(ctx context.Context) error {
	now := s.now().UnixMilli()

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(commandIndexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, commandIndexPrefix), "/", 3)
			if len(parts) != 3 {
				tx.Delete(k)
				continue
			}

			scope := request.Scope{AccountID: parts[0], Region: parts[1]}
			c, found, err := storage.Read[command](tx, commandKey(scope, parts[2]))
			if err != nil {
				return err
			}

			if !found {
				tx.Delete(k)
				continue
			}

			if c.RequestedAt+settleAfter.Milliseconds() > now {
				continue
			}

			invocations, err := loadInvocations(tx, scope, c.ID)
			if err != nil {
				return err
			}

			for _, i := range invocations {
				if !i.finished() {
					i.Status, i.StartedAt, i.FinishedAt = statusSuccess, now, now
					if writeErr := storage.Write(tx, invocationKey(scope, c.ID, i.InstanceID), i); writeErr != nil {
						return writeErr
					}
				}
			}

			c.settle(invocations)
			tx.Delete(k)
			if writeErr := storage.Write(tx, commandKey(scope, c.ID), c); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}

func loadInvocations(tx storage.ReadTx, scope request.Scope, id string) ([]*invocation, error) {
	var out []*invocation
	for _, k := range tx.Keys(invocationPrefix(scope, id)) {
		i, _, err := storage.Read[invocation](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, i)
	}

	return out, nil
}

func (s *Service) CancelCommand(ctx context.Context, in *api.CancelCommandRequest) (*api.CancelCommandResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.CommandId == nil {
		return nil, validation("CommandId is required.")
	}

	for i, id := range in.InstanceIds {
		if slices.Contains(in.InstanceIds[:i], id) {
			return nil, awserror.New("DuplicateInstanceId", "Duplicate instance ID: "+id)
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, found, readErr := storage.Read[command](tx, commandKey(m.Scope, *in.CommandId))
		if readErr != nil {
			return readErr
		}

		if !found {
			return invalidCommandID()
		}

		invocations, readErr := loadInvocations(tx, m.Scope, c.ID)
		if readErr != nil {
			return readErr
		}

		for _, id := range in.InstanceIds {
			if !slices.ContainsFunc(invocations, func(i *invocation) bool { return i.InstanceID == id }) {
				return invalidInstance(id, m.AccountID)
			}
		}

		now := s.now().UnixMilli()
		for _, i := range invocations {
			if i.finished() || (len(in.InstanceIds) > 0 && !slices.Contains(in.InstanceIds, i.InstanceID)) {
				continue
			}

			i.Status, i.FinishedAt = statusCancelled, now
			if writeErr := storage.Write(tx, invocationKey(m.Scope, c.ID, i.InstanceID), i); writeErr != nil {
				return writeErr
			}
		}

		c.settle(invocations)
		if c.Status != statusPending && c.Status != statusInProgress {
			tx.Delete(commandIndexKey(m.Scope, c.ID))
		}

		return storage.Write(tx, commandKey(m.Scope, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.CancelCommandResult{}, nil
}

// commandFilter reports whether a command matches one ListCommands filter.
type commandFilter func(c *command) bool

func parseTime(key, text string) (int64, error) {
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return 0, awserror.New("InvalidFilterValue", "The value of the filter "+key+" must be an ISO 8601 time.")
	}

	return t.UnixMilli(), nil
}

// parseCommandFilters compiles the filters of ListCommands and
// ListCommandInvocations.
func parseCommandFilters(filters []api.CommandFilter) ([]commandFilter, error) {
	var out []commandFilter
	for _, f := range filters {
		key, text := value(f.Key, ""), value(f.Value, "")
		switch key {
		case "InvokedAfter", "InvokedBefore":
			at, err := parseTime(key, text)
			if err != nil {
				return nil, err
			}

			out = append(out, func(c *command) bool {
				if key == "InvokedAfter" {
					return c.RequestedAt >= at
				}

				return c.RequestedAt <= at
			})
		case "Status":
			if !slices.Contains([]string{
				statusPending, statusInProgress, statusSuccess, statusCancelled, statusFailed,
				statusTimedOut, "Cancelling",
			}, text) {
				return nil, awserror.New("InvalidFilterValue", "The status is not valid: "+text)
			}

			out = append(out, func(c *command) bool { return c.Status == text })
		case "DocumentName":
			out = append(out, func(c *command) bool { return c.Document == text })
		case "ExecutionStage":
			if text != "Executing" && text != "Complete" {
				return nil, awserror.New("InvalidFilterValue", "ExecutionStage must be Executing or Complete.")
			}

			out = append(out, func(c *command) bool {
				return (c.Status == statusPending || c.Status == statusInProgress) == (text == "Executing")
			})
		default:
			return nil, awserror.New("InvalidFilterKey", "The following filter key is not valid: "+key)
		}
	}

	return out, nil
}

// newestFirst orders commands as AWS lists them, by the time they were sent.
func newestFirst(c *command) string {
	return fmt.Sprintf("%019d", math.MaxInt64-c.RequestedAt) + c.ID
}

// listCommands returns the commands that a request's filters keep, newest first.
func listCommands(tx storage.ReadTx, scope request.Scope, id, instance *string, filters []commandFilter) (
	[]*command,
	error,
) {
	var out []*command
	for _, k := range tx.Keys(scope.Prefix("ssm", "c")) {
		c, _, err := storage.Read[command](tx, k)
		if err != nil {
			return nil, err
		}

		if id != nil && c.ID != *id || slices.ContainsFunc(filters, func(f commandFilter) bool { return !f(c) }) {
			continue
		}

		if instance != nil {
			if _, hit := tx.Get(invocationKey(scope, c.ID, *instance)); !hit {
				continue
			}
		}

		out = append(out, c)
	}

	slices.SortFunc(out, func(a, b *command) int { return strings.Compare(newestFirst(a), newestFirst(b)) })

	return out, nil
}

func (s *Service) ListCommands(ctx context.Context, in *api.ListCommandsRequest) (*api.ListCommandsResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	filters, err := parseCommandFilters(in.Filters)
	if err != nil {
		return nil, err
	}

	out := &api.ListCommandsResult{Commands: []api.Command{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, listErr := listCommands(tx, m.Scope, in.CommandId, in.InstanceId, filters)
		if listErr != nil {
			return listErr
		}

		selected, next, pageErr := page(found, newestFirst, in.NextToken, in.MaxResults, maxCommandPage)
		if pageErr != nil {
			return pageErr
		}

		for _, c := range selected {
			out.Commands = append(out.Commands, c.output())
		}

		out.NextToken = next

		return nil
	})

	return out, err
}

func plugins(c *command, i *invocation) []api.CommandPlugin {
	var out []api.CommandPlugin
	for _, name := range c.Plugins {
		p := api.CommandPlugin{Name: new(name), Status: new(i.Status), StatusDetails: new(i.Status), Output: new("")}
		if i.StartedAt > 0 {
			p.ResponseCode = new(int64(0))
			p.ResponseStartDateTime, p.ResponseFinishDateTime = seconds(i.StartedAt), seconds(i.FinishedAt)
		} else {
			p.ResponseCode = new(int64(-1))
		}

		out = append(out, p)
	}

	return out
}

func (s *Service) ListCommandInvocations(
	ctx context.Context,
	in *api.ListCommandInvocationsRequest,
) (*api.ListCommandInvocationsResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	filters, err := parseCommandFilters(in.Filters)
	if err != nil {
		return nil, err
	}

	type entry struct {
		c *command
		i *invocation
	}

	out := &api.ListCommandInvocationsResult{CommandInvocations: []api.CommandInvocation{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, listErr := listCommands(tx, m.Scope, in.CommandId, in.InstanceId, filters)
		if listErr != nil {
			return listErr
		}

		if in.CommandId != nil && len(found) == 0 {
			if _, exists := tx.Get(commandKey(m.Scope, *in.CommandId)); !exists {
				return invalidCommandID()
			}
		}

		var entries []entry
		for _, c := range found {
			invocations, loadErr := loadInvocations(tx, m.Scope, c.ID)
			if loadErr != nil {
				return loadErr
			}

			for _, i := range invocations {
				if in.InstanceId == nil || i.InstanceID == *in.InstanceId {
					entries = append(entries, entry{c: c, i: i})
				}
			}
		}

		selected, next, pageErr := page(
			entries,
			func(e entry) string { return newestFirst(e.c) + "/" + e.i.InstanceID },
			in.NextToken,
			in.MaxResults,
			maxCommandPage,
		)
		if pageErr != nil {
			return pageErr
		}

		for _, e := range selected {
			inv := api.CommandInvocation{
				CommandId: new(e.c.ID), InstanceId: new(e.i.InstanceID), InstanceName: new(e.i.Name),
				Comment: optional(e.c.Comment), DocumentName: new(e.c.Document), DocumentVersion: new(e.c.DocVersion),
				RequestedDateTime: seconds(e.c.RequestedAt), Status: new(e.i.Status), StatusDetails: new(e.i.Status),
				ServiceRole: optional(e.c.ServiceRole), CloudWatchOutputConfig: e.c.CloudWatch,
			}
			if in.Details != nil && *in.Details {
				inv.CommandPlugins = plugins(e.c, e.i)
			}

			out.CommandInvocations = append(out.CommandInvocations, inv)
		}

		out.NextToken = next

		return nil
	})

	return out, err
}

func (s *Service) GetCommandInvocation(
	ctx context.Context,
	in *api.GetCommandInvocationRequest,
) (*api.GetCommandInvocationResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.CommandId == nil || in.InstanceId == nil {
		return nil, validation("CommandId and InstanceId are required.")
	}

	var out *api.GetCommandInvocationResult
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, found, readErr := storage.Read[command](tx, commandKey(m.Scope, *in.CommandId))
		if readErr != nil {
			return readErr
		}

		if !found {
			return invalidCommandID()
		}

		if in.PluginName != nil && !slices.Contains(c.Plugins, *in.PluginName) {
			return awserror.New("InvalidPluginName", "The plugin name is not valid for the command's document.")
		}

		i, found, readErr := storage.Read[invocation](tx, invocationKey(m.Scope, c.ID, *in.InstanceId))
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("InvocationDoesNotExist", "The command was not run on the instance.")
		}

		out = &api.GetCommandInvocationResult{
			CommandId:  new(c.ID),
			InstanceId: new(i.InstanceID),
			Comment:    optional(c.Comment),
			DocumentName: new(
				c.Document,
			),
			DocumentVersion:        new(c.DocVersion),
			PluginName:             new(value(in.PluginName, "")),
			ResponseCode:           new(int64(-1)),
			Status:                 new(i.Status),
			StatusDetails:          new(i.Status),
			StandardOutputContent:  new(""),
			StandardErrorContent:   new(""),
			ExecutionElapsedTime:   new("PT0S"),
			CloudWatchOutputConfig: c.CloudWatch,
		}
		if i.StartedAt > 0 {
			out.ResponseCode = new(int64(0))
			out.ExecutionStartDateTime = new(time.UnixMilli(i.StartedAt).UTC().Format(timestampLayout))
			out.ExecutionEndDateTime = new(time.UnixMilli(i.FinishedAt).UTC().Format(timestampLayout))
		}

		return nil
	})

	return out, err
}
