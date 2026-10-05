package cloudformation

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	changeSetCreate = "CREATE"
	changeSetUpdate = "UPDATE"
	available       = "AVAILABLE"
	// noChanges is the reason a change set without changes fails, which
	// "aws cloudformation deploy" looks for.
	noChanges = "The submitted information didn't contain changes. Submit different information to create a change set."
)

var changeSetName = regexp.MustCompile(`^[a-zA-Z][-a-zA-Z0-9]{0,127}$`)

func changeSetNotFound(name string) error {
	return awserror.New("ChangeSetNotFoundException", "ChangeSet ["+name+"] does not exist")
}

func invalidChangeSetStatus(format string, args ...any) error {
	return awserror.New("InvalidChangeSetStatusException", fmt.Sprintf(format, args...))
}

// findChangeSet reads a change set by ARN, or by name and stack.
func findChangeSet(tx storage.ReadTx, scope request.Scope, stackName, name string) (*changeSet, error) {
	if strings.HasPrefix(name, "arn:") {
		for _, k := range tx.Keys(key(scope, kindChangeSet, "")) {
			cs, _, err := storage.Read[changeSet](tx, k)
			if err != nil {
				return nil, err
			}

			if cs.ID == name {
				return cs, nil
			}
		}

		return nil, changeSetNotFound(name)
	}

	if stackName == "" {
		return nil, validation("StackName must be specified if ChangeSetName is not specified as an ARN.")
	}

	st, err := find(tx, scope, stackName)
	if err != nil {
		return nil, changeSetNotFound(name)
	}

	cs, found, err := storage.Read[changeSet](tx, key(scope, kindChangeSet, st.Name+"/"+name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, changeSetNotFound(name)
	}

	return cs, nil
}

func (s *Service) CreateChangeSet(
	ctx context.Context, in *api.CreateChangeSetInput,
) (*api.CreateChangeSetOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, typ := common.Value(in.ChangeSetName), common.Value(in.ChangeSetType)
	if !changeSetName.MatchString(name) {
		return nil, validation("1 validation error detected: Value '%s' at 'changeSetName' failed to satisfy "+
			"constraint: Member must satisfy regular expression pattern: [a-zA-Z][-a-zA-Z0-9]*", name)
	}

	switch typ {
	case "":
		typ = changeSetUpdate
	case changeSetCreate, changeSetUpdate:
	case "IMPORT":
		return nil, validation("TinyStack does not support IMPORT change sets")
	default:
		return nil, validation("Invalid ChangeSetType %s", typ)
	}

	failure, err := onFailure(in.OnStackFailure, nil)
	if err != nil {
		return nil, err
	}

	if in.OnStackFailure != nil && typ != changeSetCreate {
		return nil, validation("OnStackFailure can only be specified for CREATE change sets")
	}

	st, err := s.changeSetStack(ctx, m.Scope, common.Value(in.StackName), typ)
	if err != nil {
		return nil, err
	}

	if typ == changeSetCreate && common.Value(in.UsePreviousTemplate) {
		return nil, validation("UsePreviousTemplate cannot be used to create a stack")
	}

	_, body, params, err := s.revise(ctx, m, st, in.TemplateBody, in.TemplateURL, in.UsePreviousTemplate,
		in.Parameters, in.Capabilities)
	if err != nil {
		return nil, err
	}

	tags := st.Tags
	if in.Tags != nil {
		if tags, err = tagsOf(in.Tags); err != nil {
			return nil, err
		}
	}

	cs := &changeSet{
		ID:             changeSetARN(m.Scope, name),
		Name:           name,
		StackName:      st.Name,
		Description:    common.Value(in.Description),
		Type:           typ,
		Status:         statusCreateComplete,
		Execution:      available,
		Template:       body,
		RoleARN:        common.Value(in.RoleARN),
		OnStackFailure: failure,
		ClientToken:    common.Value(in.ClientToken),
		Parameters:     params,
		Tags:           tags,
		Capabilities:   in.Capabilities,
		Created:        s.now(),
	}
	if in.OnStackFailure == nil {
		cs.OnStackFailure = ""
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		cur, findErr := find(tx, m.Scope, st.Name)
		switch {
		case findErr == nil && typ == changeSetCreate && cur.Status != statusReview:
			return validation("Stack [%s] already exists and cannot be created again with the changeSet [%s].",
				st.Name, name)
		case findErr != nil && typ == changeSetCreate:
			cur = st
			if writeErr := s.status(tx, m.Scope, cur, statusReview, "User Initiated"); writeErr != nil {
				return writeErr
			}

			if writeErr := storage.Write(tx, key(m.Scope, kindStack, cur.Name), cur); writeErr != nil {
				return writeErr
			}
		case findErr != nil:
			return findErr
		}

		k := key(m.Scope, kindChangeSet, cur.Name+"/"+name)
		if _, exists := tx.Get(k); exists {
			return alreadyExists("ChangeSet [%s] already exists", name)
		}

		cs.StackID, cs.Changes = cur.ID, changes(cur, body, params)
		if typ == changeSetUpdate && body == cur.Template && slices.Equal(params, cur.Parameters) &&
			slices.Equal(tags, cur.Tags) {
			cs.Status, cs.Execution, cs.Reason = "FAILED", "UNAVAILABLE", noChanges
		}

		return storage.Write(tx, k, cs)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateChangeSetOutput{Id: &cs.ID, StackId: &cs.StackID}, nil
}

// changeSetStack reads the stack a change set is for, or, for a CREATE
// change set, makes the stack it creates.
func (s *Service) changeSetStack(ctx context.Context, scope request.Scope, nameOrID, typ string) (*stack, error) {
	// A deleted stack is one that does not exist.
	if st, err := s.current(ctx, scope, nameOrID); err == nil && st.Deleted == nil {
		if typ == changeSetUpdate && (st.Op != nil || !slices.Contains(updatable, st.Status)) {
			return nil, validation("Stack:%s is in %s state and can not be updated.", st.ID, st.Status)
		}

		return st, nil
	}

	if typ == changeSetUpdate {
		return nil, validation("Stack [%s] does not exist", nameOrID)
	}

	if err := checkName(nameOrID); err != nil {
		return nil, err
	}

	return &stack{
		ID: stackARN(scope, nameOrID), Name: nameOrID, Created: s.now(), Resources: map[string]*resource{},
	}, nil
}

// changes lists what applying a template with params to st would change.
// A resource whose declaration and referenced parameters are unchanged is
// left out, though a value it reads from a changed resource may change it.
func changes(st *stack, body string, params []parameter) []api.Change {
	next, err := parse(body)
	if err != nil {
		return nil
	}

	var prev *template
	if st.Template != "" {
		prev, _ = parse(st.Template)
	}

	changedParams := map[string]bool{}
	for _, p := range params {
		i := slices.IndexFunc(st.Parameters, func(old parameter) bool { return old.Key == p.Key })
		if i < 0 || st.Parameters[i] != p {
			changedParams[p.Key] = true
		}
	}

	out := []api.Change{}
	for _, logical := range slices.Sorted(maps.Keys(next.Resources)) {
		decl := next.Resources[logical]
		change := api.ResourceChange{LogicalResourceId: new(logical), ResourceType: new(decl.Type)}
		r := st.Resources[logical]
		var old *resourceDecl
		if prev != nil {
			old = prev.Resources[logical]
		}

		switch {
		case r == nil || r.Physical == "" || old == nil:
			change.Action = new("Add")
		case old.Type != decl.Type:
			change.Action, change.Replacement = new("Modify"), new("True")
		case !reflect.DeepEqual(old.Properties, decl.Properties):
			change.Action, change.Replacement = new("Modify"), new(replacement(decl, old))
		case mentions(decl.Properties, changedParams):
			change.Action, change.Replacement = new("Modify"), new("Conditional")
		default:
			continue
		}

		if r != nil && r.Physical != "" {
			change.PhysicalResourceId = new(r.Physical)
		}

		if common.Value(change.Action) == "Modify" {
			change.Scope = []string{keyProperties}
		}

		out = append(out, api.Change{Type: new("Resource"), ResourceChange: &change})
	}

	for _, logical := range slices.Sorted(maps.Keys(st.Resources)) {
		if _, kept := next.Resources[logical]; kept {
			continue
		}

		r := st.Resources[logical]
		out = append(out, api.Change{Type: new("Resource"), ResourceChange: &api.ResourceChange{
			Action: new(
				"Remove",
			),
			LogicalResourceId:  new(logical),
			PhysicalResourceId: new(r.Physical),
			ResourceType:       new(r.Type),
		}})
	}

	return out
}

// replacement says whether changing old's properties to decl's replaces the
// resource.
func replacement(decl, old *resourceDecl) string {
	typ := types[decl.Type]
	if typ.update == nil {
		return "True"
	}

	for _, k := range typ.replace {
		if !reflect.DeepEqual(decl.Properties[k], old.Properties[k]) {
			return "True"
		}
	}

	return "False"
}

// mentions reports whether v refers to any of params.
func mentions(v any, params map[string]bool) bool {
	if len(params) == 0 {
		return false
	}

	data, _ := json.Marshal(v)
	text := string(data)
	for p := range params {
		if strings.Contains(text, `"Ref":"`+p+`"`) || strings.Contains(text, "${"+p+"}") {
			return true
		}
	}

	return false
}

func (s *Service) DescribeChangeSet(
	ctx context.Context, in *api.DescribeChangeSetInput,
) (*api.DescribeChangeSetOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var cs *changeSet
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var findErr error
		cs, findErr = findChangeSet(tx, m.Scope, common.Value(in.StackName), common.Value(in.ChangeSetName))
		return findErr
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(cs.Changes, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeChangeSetOutput{
		ChangeSetName: &cs.Name, ChangeSetId: &cs.ID, StackId: &cs.StackID, StackName: &cs.StackName,
		Parameters: parametersOf(cs.Parameters), CreationTime: new(cs.Created), ExecutionStatus: &cs.Execution,
		Status: &cs.Status, Capabilities: cs.Capabilities, Tags: apiTags(cs.Tags), Changes: items, NextToken: next,
	}
	if cs.Description != "" {
		out.Description = &cs.Description
	}

	if cs.Reason != "" {
		out.StatusReason = &cs.Reason
	}

	if cs.OnStackFailure != "" {
		out.OnStackFailure = &cs.OnStackFailure
	}

	return out, nil
}

func (s *Service) ExecuteChangeSet(
	ctx context.Context, in *api.ExecuteChangeSetInput,
) (*api.ExecuteChangeSetOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		cs, findErr := findChangeSet(tx, m.Scope, common.Value(in.StackName), common.Value(in.ChangeSetName))
		if findErr != nil {
			return findErr
		}

		if cs.Execution != available {
			return invalidChangeSetStatus("ChangeSet [%s] cannot be executed in its current status of [%s]",
				cs.ID, cs.Execution)
		}

		st, findErr := find(tx, m.Scope, cs.StackName)
		if findErr != nil {
			return findErr
		}

		if st.Op != nil || (st.Status != statusReview && !slices.Contains(updatable, st.Status)) {
			return validation("Stack:%s is in %s state and can not be updated.", st.ID, st.Status)
		}

		return s.execute(tx, m, st, cs, in)
	})
	if err != nil {
		return nil, err
	}

	s.poke()
	return &api.ExecuteChangeSetOutput{}, nil
}

// execute starts the operation a change set describes.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) execute(
	tx storage.WriteTx, m request.Metadata, st *stack, cs *changeSet, in *api.ExecuteChangeSetInput,
) error {
	for _, k := range tx.Keys(key(m.Scope, kindChangeSet, st.Name+"/")) {
		if k != key(m.Scope, kindChangeSet, st.Name+"/"+cs.Name) {
			tx.Delete(k)
		}
	}

	cs.Execution = "EXECUTE_IN_PROGRESS"
	if err := storage.Write(tx, key(m.Scope, kindChangeSet, st.Name+"/"+cs.Name), cs); err != nil {
		return err
	}

	description := ""
	if t, err := parse(cs.Template); err == nil {
		description = t.Description
	}

	op := &operation{
		Phase:     phaseApply,
		ChangeSet: cs.Name,
		Token:     common.Value(in.ClientRequestToken),
		OnFailure: onFailureRollback,
	}
	if common.Value(in.DisableRollback) {
		op.OnFailure = onFailureNothing
	}

	status := statusUpdateInProgress
	if st.Status == statusReview {
		op.Action, status = actionCreate, statusCreateInProgress
		if cs.OnStackFailure != "" && in.DisableRollback == nil {
			op.OnFailure = cs.OnStackFailure
		}

		st.DisableRollback = op.OnFailure == onFailureNothing
	} else {
		now := s.now()
		op.Action, st.Updated = actionUpdate, &now
		op.Previous = &revision{
			Template: st.Template, Description: st.Description, Parameters: st.Parameters, Tags: st.Tags,
		}
	}

	st.Template, st.Description, st.Parameters, st.Tags = cs.Template, description, cs.Parameters, cs.Tags
	st.Capabilities, st.RoleARN, st.ChangeSetID, st.Endpoint = cs.Capabilities, cs.RoleARN, cs.ID, m.Endpoint
	return s.begin(tx, m.Scope, st, op, status)
}

func (s *Service) DeleteChangeSet(
	ctx context.Context, in *api.DeleteChangeSetInput,
) (*api.DeleteChangeSetOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		cs, findErr := findChangeSet(tx, m.Scope, common.Value(in.StackName), common.Value(in.ChangeSetName))
		if findErr != nil {
			return findErr
		}

		if cs.Execution == "EXECUTE_IN_PROGRESS" {
			return invalidChangeSetStatus("Cannot delete ChangeSet in status %s", cs.Execution)
		}

		tx.Delete(key(m.Scope, kindChangeSet, cs.StackName+"/"+cs.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteChangeSetOutput{}, nil
}

func (s *Service) ListChangeSets(
	ctx context.Context, in *api.ListChangeSetsInput,
) (*api.ListChangeSetsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	summaries := []api.ChangeSetSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		st, findErr := find(tx, m.Scope, common.Value(in.StackName))
		if findErr != nil {
			return findErr
		}

		for _, k := range tx.Keys(key(m.Scope, kindChangeSet, st.Name+"/")) {
			cs, _, findErr := storage.Read[changeSet](tx, k)
			if findErr != nil {
				return findErr
			}

			summary := api.ChangeSetSummary{
				StackId: &cs.StackID, StackName: &cs.StackName, ChangeSetId: &cs.ID, ChangeSetName: &cs.Name,
				ExecutionStatus: &cs.Execution, Status: &cs.Status, CreationTime: new(cs.Created),
			}
			if cs.Reason != "" {
				summary.StatusReason = &cs.Reason
			}

			if cs.Description != "" {
				summary.Description = &cs.Description
			}

			summaries = append(summaries, summary)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(summaries, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.ListChangeSetsOutput{Summaries: items, NextToken: next}, nil
}
