package cloudformation

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// runInterval is how often the runner looks for stacks with work to do.
var runInterval = time.Second

const (
	stepCreate  = "create"
	stepUpdate  = "update"
	stepReplace = "replace"
	// stepSwap creates a resource's replacement; the old one is deleted
	// during cleanup.
	stepSwap = "swap"
	// stepDiscard deletes a resource an update replaced.
	stepDiscard = "discard"
	stepRemove  = "remove"
	// stepRetain leaves a resource out of the stack without deleting it.
	stepRetain = "retain"
	// stepForget drops a resource that was never created.
	stepForget = "forget"
	stepFinish = "finish"
	stepFail   = "fail"
)

// step is the next thing a stack's operation does.
type step struct {
	props   props
	kind    string
	logical string
	typ     string
	reason  string
	// decl is the template's declaration of the resource being applied.
	decl *resourceDecl
	// settled are the declarations of a finished apply phase's resources.
	settled map[string]*resourceDecl
	outputs []output
	imports []string
}

type stackRef struct {
	scope request.Scope
	name  string
}

func (s *Service) runner(ctx context.Context) {
	t := time.NewTicker(runInterval)
	defer t.Stop()
	for {
		s.visit(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// visit advances each stack with an operation in progress that is not
// already advancing.
func (s *Service) visit(ctx context.Context) {
	var refs []stackRef
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 3)
			if len(parts) == 3 {
				refs = append(refs, stackRef{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})

	for _, ref := range refs {
		id := ref.scope.AccountID + "/" + ref.scope.Region + "/" + ref.name
		s.mu.Lock()
		if s.busy[id] {
			s.mu.Unlock()
			continue
		}

		s.busy[id] = true
		s.mu.Unlock()
		s.bg.Go(func(ctx context.Context) {
			defer func() {
				s.mu.Lock()
				delete(s.busy, id)
				s.mu.Unlock()
			}()

			s.advance(ctx, ref)
		})
	}
}

// advance takes a stack's steps until its operation ends or the service
// stops.
func (s *Service) advance(ctx context.Context, ref stackRef) {
	for ctx.Err() == nil {
		more, err := s.next(ctx, ref)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Error("cloudformation stack", "stack", ref.name, "error", err)
			}

			return
		}

		if !more {
			return
		}
	}
}

// next plans and takes a stack's next step, reporting whether another
// follows.
func (s *Service) next(ctx context.Context, ref stackRef) (bool, error) {
	var st *stack
	var next step
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		st, _, err = storage.Read[stack](tx, ref.scope.Key(service, kindStack, ref.name))
		if err != nil || st == nil || st.Op == nil {
			return err
		}

		if st.Op.Pending == nil {
			next = s.plan(tx, ref.scope, st)
		}
		return nil
	}); err != nil {
		return false, err
	}

	if st == nil || st.Op == nil {
		return false, s.store.Update(ctx, func(tx storage.WriteTx) error {
			cur, _, err := storage.Read[stack](tx, ref.scope.Key(service, kindStack, ref.name))
			if err == nil && (cur == nil || cur.Op == nil) {
				tx.Delete(indexKey(ref.scope, ref.name))
			}

			return err
		})
	}

	if st.Op.Pending != nil {
		return s.resumePending(ctx, ref, st)
	}

	switch next.kind {
	case stepFinish:
		return true, s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
			return s.finish(tx, ref.scope, cur, &next)
		})
	case stepFail:
		return true, s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
			if err := s.resourceFailed(tx, ref.scope, cur, &next, next.reason); err != nil {
				return err
			}

			return s.fail(tx, ref.scope, cur, next.logical, next.reason)
		})
	case stepRetain, stepForget:
		return true, s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
			return s.drop(tx, ref.scope, cur, &next)
		})
	}

	if err := s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
		return s.start(tx, ref.scope, cur, &next)
	}); err != nil {
		return false, err
	}

	if resourceTypeOf(stepResourceType(st, &next)).pending {
		return s.beginPending(ctx, ref, st, &next)
	}

	physical, attributes, err := s.perform(ctx, ref.scope, st, &next)
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}

	return true, s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
		return s.record(tx, ref.scope, cur, &next, physical, attributes, err)
	})
}

// commit changes a stack with an operation in progress in a transaction
// that outlives the service's context, so a step that ran is recorded.
func (s *Service) commit(ctx context.Context, ref stackRef, change func(tx storage.WriteTx, st *stack) error) error {
	return s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		st, found, err := storage.Read[stack](tx, ref.scope.Key(service, kindStack, ref.name))
		if err != nil || !found || st.Op == nil {
			return err
		}

		if err = change(tx, st); err != nil || st.Deleted != nil {
			return err
		}

		return storage.Write(tx, ref.scope.Key(service, kindStack, st.Name), st)
	})
}

// plan works out a stack's next step.
func (s *Service) plan(tx storage.ReadTx, scope request.Scope, st *stack) step {
	op := st.Op
	timeout := time.Duration(st.Timeout) * time.Minute
	if op.Action == actionCreate && op.Phase == phaseApply && !op.Rollback && st.Timeout > 0 &&
		s.now().After(op.Started.Add(timeout)) {
		return step{kind: stepFail, reason: "Stack creation time exceeded the specified timeout."}
	}

	if op.Phase == phaseApply {
		return s.planApply(tx, scope, st)
	}

	return s.planRemove(tx, scope, st)
}

// planApply finds the first resource, in dependency order, that does not
// match the template, or finishes the phase.
func (s *Service) planApply(tx storage.ReadTx, scope request.Scope, st *stack) step {
	t, err := parse(st.Template)
	if err != nil {
		return step{kind: stepFail, reason: awserror.From(err).Message}
	}

	e := newEvaluator(t, scope, st, st.Parameters, importer(tx, scope))
	settled := map[string]*resourceDecl{}
	for _, logical := range t.order {
		decl := t.Resources[logical]
		holds, loadErr := e.active(decl.Condition)
		if loadErr != nil {
			return step{kind: stepFail, logical: logical, typ: decl.Type, reason: awserror.From(loadErr).Message}
		}

		if !holds {
			continue
		}

		p, loadErr := e.properties(decl)
		if loadErr != nil {
			return step{kind: stepFail, logical: logical, typ: decl.Type, reason: awserror.From(loadErr).Message}
		}

		settled[logical] = decl
		next := step{logical: logical, typ: decl.Type, props: p, decl: decl}
		r := st.Resources[logical]
		switch {
		case r == nil || r.Physical == "":
			next.kind = stepCreate
		case r.Type != decl.Type:
			next.kind = stepReplace
		case (r.Status == statusCreateComplete || r.Status == statusUpdateComplete) &&
			(len(r.Properties) == 0 && len(p) == 0 || reflect.DeepEqual(r.Properties, p)):
			continue
		case resourceTypeOf(decl.Type).replaces(r.Properties, p):
			next.kind = stepReplace
			if resourceTypeOf(decl.Type).createsFirst(r.Properties, p) {
				next.kind = stepSwap
			}
		default:
			next.kind = stepUpdate
		}

		return next
	}

	outputs, err := e.outputs()
	if err != nil {
		return step{kind: stepFail, reason: awserror.From(err).Message}
	}

	return step{
		kind: stepFinish, settled: settled, outputs: outputs, imports: slices.Sorted(maps.Keys(e.imported)),
	}
}

// planRemove finds a resource to delete that no remaining resource depends
// on: during cleanup, one the template no longer has; otherwise, any.
func (s *Service) planRemove(tx storage.ReadTx, scope request.Scope, st *stack) step {
	if len(st.Replaced) > 0 {
		old := st.Replaced[0]
		return step{kind: stepDiscard, logical: old.Logical, typ: old.Resource.Type}
	}

	keep := map[string]bool{}
	if st.Op.Phase == phaseCleanup {
		keep = kept(tx, scope, st)
	}

	for _, logical := range slices.Sorted(maps.Keys(st.Resources)) {
		if keep[logical] || dependedOn(st, logical) {
			continue
		}

		r := st.Resources[logical]
		next := step{logical: logical, typ: r.Type}
		switch {
		case r.Physical == "":
			next.kind = stepForget
		case slices.Contains(st.Op.Retain, logical), r.Policy == policyRetain,
			r.Policy == "RetainExceptOnCreate" && (st.Op.Action != actionCreate || !st.Op.Rollback):
			next.kind = stepRetain
		default:
			next.kind = stepRemove
		}

		return next
	}

	return step{kind: stepFinish}
}

// kept returns the resources a stack's template has whose conditions hold,
// or every resource if that cannot be worked out.
func kept(tx storage.ReadTx, scope request.Scope, st *stack) map[string]bool {
	out := map[string]bool{}
	t, err := parse(st.Template)
	if err != nil {
		for logical := range st.Resources {
			out[logical] = true
		}

		return out
	}

	e := newEvaluator(t, scope, st, st.Parameters, importer(tx, scope))
	for logical, decl := range t.Resources {
		holds, err := e.active(decl.Condition)
		if err != nil || holds {
			out[logical] = true
		}
	}

	return out
}

func dependedOn(st *stack, logical string) bool {
	for other, r := range st.Resources {
		if other != logical && slices.Contains(r.Deps, logical) {
			return true
		}
	}

	return false
}

// importer looks up exports in a stack's account and region.
func importer(tx storage.ReadTx, scope request.Scope) func(string) (string, error) {
	return func(name string) (string, error) {
		e, found, err := storage.Read[export](tx, scope.Key(service, kindExport, name))
		if err != nil {
			return "", err
		}

		if !found {
			return "", validation("No export named %s found.", name)
		}

		return e.Value, nil
	}
}

// perform calls the services a step needs, outside transactions.
func (s *Service) perform(ctx context.Context,
	scope request.Scope, st *stack, next *step,
) (physical string, attributes map[string]any, err error) {
	c := &client{
		caller:   s.caller,
		mq:       s.mq,
		msk:      s.msk,
		mwaa:     s.mwaa,
		dataSync: s.dataSync,
		transfer: s.transfer,
		location: s.location,
		s3tables: s.s3tables,
		scope:    scope,
		endpoint: st.Endpoint,
		stack:    st.Name,
		logical:  next.logical,
	}
	r := st.Resources[next.logical]
	if r != nil && r.Type == mqBrokerType && !st.Op.Rollback &&
		(next.kind == stepSwap || next.kind == stepUpdate) &&
		props(r.Properties).has(mqEncryptionOptions) && !next.props.has(mqEncryptionOptions) {
		return "", nil, validation(
			"Removing MQ EncryptionOptions is unsupported; retain the explicit AWS-owned-key choice",
		)
	}
	if r != nil && r.Type == mskClusterType && !st.Op.Rollback && (next.kind == stepSwap || next.kind == stepUpdate) {
		if _, e := mskParams(next.props); e != nil {
			return "", nil, e
		}
		if e := mskUnsupportedChanges(props(r.Properties), next.props); e != nil {
			return "", nil, e
		}
	}
	if r != nil && r.Type == mwaaEnvironmentType && !st.Op.Rollback &&
		(next.kind == stepSwap || next.kind == stepUpdate) {
		if _, e := mwaaParams(next.props); e != nil {
			return "", nil, e
		}
		if e := mwaaUnsupportedChanges(props(r.Properties), next.props); e != nil {
			return "", nil, e
		}
	}
	if r != nil && isTransferResource(r.Type) && !st.Op.Rollback && (next.kind == stepSwap || next.kind == stepUpdate) {
		if e := transferPreflight(r.Type, props(r.Properties), next.props, c.scope); e != nil {
			return "", nil, e
		}
	}
	if r != nil && r.Type == locationTrackerType && !st.Op.Rollback &&
		(next.kind == stepSwap || next.kind == stepUpdate) {
		if e := locationPreflight(props(r.Properties), next.props); e != nil {
			return "", nil, e
		}
	}
	if r != nil && isS3TablesResource(r.Type) && !st.Op.Rollback && (next.kind == stepSwap || next.kind == stepUpdate) {
		if _, e := s3tablesParams(r.Type, next.props, c.scope); e != nil {
			return "", nil, e
		}
	}
	switch next.kind {
	case stepCreate, stepSwap:
		if id, attrs, ok := s3tablesRestoreReplacement(st, next); ok {
			live, e := s3tablesRead(ctx, c, &resource{Type: next.typ, Physical: id, Attributes: attrs})
			return id, live, e
		}
		if id, attrs, ok := locationRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := transferRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := mwaaRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := mskRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := clusterRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := efsRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := batchRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := mqRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		if id, attrs, ok := codebuildRestoreReplacement(st, next); ok {
			return id, attrs, nil
		}
		return types[next.typ].create(ctx, c, next.props)
	case stepDiscard:
		retired := st.Replaced[0].Resource
		if isS3TablesResource(retired.Type) && sameS3TablesIdentity(r, retired) {
			return "", nil, nil
		}
		// A rollback can restore an association, EFS or Batch record retired by an
		// earlier swap. Cleanup must keep the incarnation owned by the active
		// resource rather than deleting the restored record.
		restored := retired.Type == "AWS::WAFv2::WebACLAssociation" ||
			isEFSResource(retired.Type) || isBatchResource(retired.Type) || isDBClusterResource(retired.Type) ||
			retired.Type == codebuildProjectType || retired.Type == mqBrokerType || retired.Type == mskClusterType ||
			retired.Type == mwaaEnvironmentType || isTransferResource(retired.Type) || retired.Type == locationTrackerType
		if restored && r != nil &&
			r.Type == retired.Type &&
			r.Physical == retired.Physical {
			return "", nil, nil
		}
		return "", nil, types[next.typ].remove(ctx, c, retired)
	case stepUpdate:
		if st.Op.Rollback && isS3TablesResource(r.Type) {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, e := s3tablesRead(ctx, c, r)
				return r.Physical, attrs, e
			}
			if attrs, found, e := s3tablesRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, e
			}
		}
		if st.Op.Rollback && r.Type == locationTrackerType {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, e := locationRead(ctx, c, r.Physical)
				return r.Physical, attrs, e
			}
			if attrs, found, e := locationRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, e
			}
		}
		if st.Op.Rollback && isTransferResource(r.Type) {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, e := transferRead(ctx, c, r)
				return r.Physical, attrs, e
			}
			if attrs, found, e := transferRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, e
			}
		}
		if st.Op.Rollback && r.Type == mwaaEnvironmentType {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, e := mwaaRead(ctx, c, r.Physical)
				return r.Physical, attrs, e
			}
			if attrs, found, e := mwaaRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, e
			}
		}
		if st.Op.Rollback && r.Type == mskClusterType {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, e := mskRead(ctx, c, r.Physical)
				return r.Physical, attrs, e
			}
			if attrs, found, e := mskRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, e
			}
		}
		if st.Op.Rollback && r.Type == mqBrokerType {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, readErr := mqRead(ctx, c, r.Physical)
				return r.Physical, attrs, readErr
			}
			if attrs, found, restoreErr := mqRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, restoreErr
			}
		}
		if st.Op.Rollback && r.Type == codebuildProjectType {
			if reflect.DeepEqual(props(r.Properties), next.props) {
				attrs, readErr := codebuildRead(ctx, c, r.Physical)
				return r.Physical, attrs, readErr
			}
			if attrs, found, restoreErr := codebuildRestoreUpdate(ctx, c, r, next.props); found {
				return r.Physical, attrs, restoreErr
			}
		}
		attributes, err := types[next.typ].update(ctx, c, r, next.props)
		return r.Physical, attributes, err
	case stepReplace:
		if next.decl.UpdateReplacePolicy == policyRetain {
			return "", nil, nil
		}

		return "", nil, types[r.Type].remove(ctx, c, r)
	default:
		return "", nil, types[r.Type].remove(ctx, c, r)
	}
}

const (
	statusCreateInProgress = "CREATE_IN_PROGRESS"
	statusCreateComplete   = "CREATE_COMPLETE"
	statusCreateFailed     = "CREATE_FAILED"
	statusUpdateInProgress = "UPDATE_IN_PROGRESS"
	statusUpdateComplete   = "UPDATE_COMPLETE"
	statusUpdateFailed     = "UPDATE_FAILED"
	statusDeleteInProgress = "DELETE_IN_PROGRESS"
	statusDeleteComplete   = "DELETE_COMPLETE"
	statusDeleteFailed     = "DELETE_FAILED"
	statusDeleteSkipped    = "DELETE_SKIPPED"
	statusReview           = "REVIEW_IN_PROGRESS"
)

// start records that a step is starting.
func (s *Service) start(tx storage.WriteTx, scope request.Scope, st *stack, next *step) error {
	if next.kind == stepDiscard {
		old := st.Replaced[0].Resource
		return s.event(tx, scope, st, next.logical, old.Physical, old.Type, statusDeleteInProgress, "", nil)
	}

	r := st.Resources[next.logical]
	status, reason := statusUpdateInProgress, ""
	switch next.kind {
	case stepCreate:
		status = statusCreateInProgress
		if r == nil {
			r = &resource{Type: next.typ}
			st.Resources[next.logical] = r
		}
	case stepReplace, stepSwap:
		reason = "Requested update requires the creation of a new physical resource; hence creating one."
	case stepRemove:
		status = statusDeleteInProgress
	}

	r.Status, r.Reason, r.Updated = status, "", s.now()
	return s.event(tx, scope, st, next.logical, r.Physical, next.typ, status, reason, next.props)
}

// record records a step's result.
func (s *Service) record(
	tx storage.WriteTx, scope request.Scope, st *stack, next *step, physical string, attributes map[string]any,
	failure error,
) error {
	if next.kind == stepDiscard {
		return s.discarded(tx, scope, st, failure)
	}

	r := st.Resources[next.logical]
	if r == nil {
		return nil
	}

	if failure != nil {
		reason := failureReason(failure)
		if next.kind == stepRemove && st.Op.Phase == phaseCleanup {
			delete(st.Resources, next.logical)
			return s.event(tx, scope, st, next.logical, r.Physical, r.Type, statusDeleteFailed, reason, nil)
		}

		if err := s.resourceFailed(tx, scope, st, next, reason); err != nil {
			return err
		}

		return s.fail(tx, scope, st, next.logical, reason)
	}

	now := s.now()
	switch next.kind {
	case stepCreate, stepUpdate, stepSwap:
		status := statusCreateComplete
		if next.kind != stepCreate {
			status = statusUpdateComplete
		}

		identityChanged := physical != r.Physical
		if isS3TablesResource(r.Type) {
			identityChanged = !sameS3TablesIdentity(
				r,
				&resource{Type: next.typ, Physical: physical, Attributes: attributes},
			)
		}
		if (next.kind == stepSwap || next.kind == stepUpdate && isCustomResource(next.typ)) && identityChanged &&
			next.decl.UpdateReplacePolicy != policyRetain {
			old := *r
			st.Replaced = append(st.Replaced, &replaced{Logical: next.logical, Resource: &old})
			clusterOrderRetired(st)
			efsOrderRetired(st)
			batchOrderRetired(st)
			s3tablesOrderRetired(st)
		}

		r.Type, r.Physical, r.Status, r.Reason, r.Updated = next.typ, physical, status, "", now
		r.Properties, r.Attributes = next.props, attributes
		clusterRefreshMemberAttributes(st, r)
		settle(r, next.decl)
		return s.event(tx, scope, st, next.logical, physical, next.typ, status, "", next.props)
	case stepReplace:
		// The old resource is gone; the next step creates the new one.
		r.Type, r.Physical, r.Attributes, r.Updated = next.typ, "", nil, now
		return nil
	default:
		delete(st.Resources, next.logical)
		return s.event(tx, scope, st, next.logical, r.Physical, r.Type, statusDeleteComplete, "", nil)
	}
}

// discarded records the deletion of the first resource an update replaced.
// A failure fails a stack's deletion; during cleanup, the resource is left
// behind, as with the resources the template no longer has.
func (s *Service) discarded(tx storage.WriteTx, scope request.Scope, st *stack, failure error) error {
	old := st.Replaced[0]
	if failure != nil && st.Op.Phase == phaseRemove {
		reason := failureReason(failure)
		if err := s.event(
			tx, scope, st, old.Logical, old.Resource.Physical, old.Resource.Type, statusDeleteFailed, reason, nil,
		); err != nil {
			return err
		}

		return s.fail(tx, scope, st, old.Logical, reason)
	}

	st.Replaced = st.Replaced[1:]
	status, reason := statusDeleteComplete, ""
	if failure != nil {
		status, reason = statusDeleteFailed, failureReason(failure)
	}

	return s.event(tx, scope, st, old.Logical, old.Resource.Physical, old.Resource.Type, status, reason, nil)
}

// failureReason describes why a service call failed: its error code and
// message.
func failureReason(err error) string {
	var e *awserror.Error
	switch {
	case !errors.As(err, &e):
		return err.Error()
	case e.Code == "ValidationError":
		return e.Message
	default:
		return e.Code + ": " + e.Message
	}
}

// settle records what a resource's declaration says about deleting it.
func settle(r *resource, decl *resourceDecl) {
	r.Deps, r.Policy, r.Metadata = decl.deps, decl.DeletionPolicy, ""
	if decl.Metadata != nil {
		data, _ := json.Marshal(decl.Metadata)
		r.Metadata = string(data)
	}
}

// resourceFailed marks the resource a failed step was for.
func (s *Service) resourceFailed(tx storage.WriteTx, scope request.Scope, st *stack, next *step, reason string) error {
	if next.logical == "" {
		return nil
	}

	r := st.Resources[next.logical]
	if r == nil {
		return s.event(tx, scope, st, next.logical, "", next.typ, statusCreateFailed, reason, nil)
	}

	status := statusUpdateFailed
	switch {
	case next.kind == stepRemove:
		status = statusDeleteFailed
	case next.kind == stepCreate || r.Physical == "":
		status = statusCreateFailed
	}

	r.Status, r.Reason, r.Updated = status, reason, s.now()
	return s.event(tx, scope, st, next.logical, r.Physical, r.Type, status, reason, next.props)
}

// drop leaves a resource out of the stack without deleting it.
func (s *Service) drop(tx storage.WriteTx, scope request.Scope, st *stack, next *step) error {
	r := st.Resources[next.logical]
	delete(st.Resources, next.logical)
	status := statusDeleteComplete
	if next.kind == stepRetain {
		status = statusDeleteSkipped
	}

	return s.event(tx, scope, st, next.logical, r.Physical, r.Type, status, "", nil)
}

// finish ends an operation's phase.
func (s *Service) finish(tx storage.WriteTx, scope request.Scope, st *stack, next *step) error {
	op := st.Op
	switch op.Phase {
	case phaseApply:
		if err := s.publish(tx, scope, st, next.outputs); err != nil {
			if op.Rollback {
				return s.end(tx, scope, st, "UPDATE_ROLLBACK_FAILED", awserror.From(err).Message)
			}

			return s.fail(tx, scope, st, "", awserror.From(err).Message)
		}

		st.Outputs, st.Imports = next.outputs, next.imports
		for logical, decl := range next.settled {
			if r := st.Resources[logical]; r != nil {
				settle(r, decl)
			}
		}

		if op.Action == actionCreate {
			return s.end(tx, scope, st, statusCreateComplete, "")
		}

		op.Phase = phaseCleanup
		status := "UPDATE_COMPLETE_CLEANUP_IN_PROGRESS"
		if op.Rollback {
			status = "UPDATE_ROLLBACK_COMPLETE_CLEANUP_IN_PROGRESS"
		}

		st.Status, st.Reason = status, ""
		return s.event(tx, scope, st, st.Name, st.ID, stackType, status, "", nil)
	case phaseCleanup:
		if op.Rollback {
			return s.end(tx, scope, st, "UPDATE_ROLLBACK_COMPLETE", "")
		}

		return s.end(tx, scope, st, statusUpdateComplete, "")
	default:
		if op.Action == actionCreate && op.Rollback {
			if err := s.publish(tx, scope, st, nil); err != nil {
				return err
			}

			st.Outputs, st.Imports = nil, nil
			return s.end(tx, scope, st, "ROLLBACK_COMPLETE", "")
		}

		return s.retire(tx, scope, st)
	}
}

// fail moves an operation that failed to its rollback, or ends it.
func (s *Service) fail(tx storage.WriteTx, scope request.Scope, st *stack, logical, reason string) error {
	op := st.Op
	failed := reason
	switch {
	case op.Phase == phaseApply && !op.Rollback && op.Action == actionCreate:
		if logical != "" {
			failed = "The following resource(s) failed to create: [" + logical + "]. "
		}

		switch op.OnFailure {
		case onFailureNothing:
			return s.end(tx, scope, st, statusCreateFailed, failed)
		case "DELETE":
			op.Phase, op.Failed = phaseRemove, logical
			return s.status(tx, scope, st, statusDeleteInProgress, failed)
		default:
			op.Phase, op.Rollback, op.Failed = phaseRemove, true, logical
			return s.status(tx, scope, st, "ROLLBACK_IN_PROGRESS", failed+"Rollback requested by user.")
		}
	case op.Phase == phaseApply && !op.Rollback:
		if logical != "" {
			failed = "The following resource(s) failed to update: [" + logical + "]. "
		}

		if op.OnFailure == onFailureNothing {
			return s.end(tx, scope, st, statusUpdateFailed, failed)
		}

		prev := op.Previous
		st.Template, st.Description, st.Parameters, st.Tags = prev.Template, prev.Description, prev.Parameters, prev.Tags
		op.Rollback, op.Failed = true, logical
		return s.status(tx, scope, st, "UPDATE_ROLLBACK_IN_PROGRESS", failed)
	case op.Phase == phaseApply:
		return s.end(tx, scope, st, "UPDATE_ROLLBACK_FAILED", reason)
	case op.Action == actionCreate && op.Rollback:
		return s.end(tx, scope, st, "ROLLBACK_FAILED", reason)
	default:
		return s.end(tx, scope, st, statusDeleteFailed, reason)
	}
}

// status changes a stack's status and records the event.
func (s *Service) status(tx storage.WriteTx, scope request.Scope, st *stack, status, reason string) error {
	st.Status, st.Reason = status, reason
	return s.event(tx, scope, st, st.Name, st.ID, stackType, status, reason, nil)
}

// end ends a stack's operation in status.
func (s *Service) end(tx storage.WriteTx, scope request.Scope, st *stack, status, reason string) error {
	if err := s.status(tx, scope, st, status, reason); err != nil {
		return err
	}

	if err := s.executed(tx, scope, st, status == statusCreateComplete || status == statusUpdateComplete); err != nil {
		return err
	}

	st.Op = nil
	tx.Delete(indexKey(scope, st.Name))
	return nil
}

// executed records how the change set an operation executes ended.
func (s *Service) executed(tx storage.WriteTx, scope request.Scope, st *stack, succeeded bool) error {
	if st.Op == nil || st.Op.ChangeSet == "" {
		return nil
	}

	k := scope.Key(service, kindChangeSet, st.Name+"/"+st.Op.ChangeSet)
	cs, found, err := storage.Read[changeSet](tx, k)
	if err != nil || !found {
		return err
	}

	cs.Execution = "EXECUTE_FAILED"
	if succeeded {
		cs.Execution = "EXECUTE_COMPLETE"
	}

	return storage.Write(tx, k, cs)
}

// retire ends a stack's deletion: its record moves to the deleted stacks,
// and its exports and change sets go.
func (s *Service) retire(tx storage.WriteTx, scope request.Scope, st *stack) error {
	if err := s.publish(tx, scope, st, nil); err != nil {
		return err
	}

	if st.Op != nil {
		if err := s.end(tx, scope, st, statusDeleteComplete, ""); err != nil {
			return err
		}
	} else if err := s.status(tx, scope, st, statusDeleteComplete, ""); err != nil {
		return err
	}

	now := s.now()
	st.Deleted, st.Outputs, st.Imports, st.Resources = &now, nil, nil, map[string]*resource{}
	for _, k := range tx.Keys(scope.Key(service, kindChangeSet, st.Name+"/")) {
		tx.Delete(k)
	}

	tx.Delete(scope.Key(service, kindStack, st.Name))
	tx.Delete(indexKey(scope, st.Name))
	if err := storage.Write(tx, scope.Key(service, kindDeleted, stackUUID(st.ID)), st); err != nil {
		return err
	}

	return purge(tx, scope, now)
}

// purge removes deleted stacks, and their events, after the retention
// period.
func purge(tx storage.WriteTx, scope request.Scope, now time.Time) error {
	for _, k := range tx.Keys(scope.Key(service, kindDeleted, "")) {
		old, _, err := storage.Read[stack](tx, k)
		if err != nil {
			return err
		}

		if old.Deleted == nil || now.Sub(*old.Deleted) < retention {
			continue
		}

		tx.Delete(k)
		for _, e := range tx.Keys(scope.Key(service, kindEvent, stackUUID(old.ID)+"/")) {
			tx.Delete(e)
		}
	}

	return nil
}

// publish replaces a stack's exports with those of outputs, failing if
// another stack exports one of the names or imports one that changes.
func (s *Service) publish(tx storage.WriteTx, scope request.Scope, st *stack, outputs []output) error {
	next := map[string]string{}
	for _, o := range outputs {
		if o.Export != "" {
			next[o.Export] = o.Value
		}
	}

	for _, o := range st.Outputs {
		if o.Export == "" {
			continue
		}

		if v, ok := next[o.Export]; ok && v == o.Value {
			continue
		}

		importers, err := importers(tx, scope, o.Export, st.Name)
		if err != nil {
			return err
		}

		if len(importers) > 0 {
			return validation("Cannot update export %s as it is in use by %s", o.Export, strings.Join(importers, ", "))
		}

		if _, kept := next[o.Export]; !kept {
			tx.Delete(scope.Key(service, kindExport, o.Export))
		}
	}

	for _, name := range slices.Sorted(maps.Keys(next)) {
		e, found, err := storage.Read[export](tx, scope.Key(service, kindExport, name))
		if err != nil {
			return err
		}

		if found && e.StackID != st.ID {
			return validation("Export with name %s is already exported by stack %s", name, e.StackName)
		}

		err = storage.Write(
			tx,
			scope.Key(service, kindExport, name),
			export{StackID: st.ID, StackName: st.Name, Value: next[name]},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// importers returns the stacks, other than except, that import an export.
func importers(tx storage.ReadTx, scope request.Scope, name, except string) ([]string, error) {
	var out []string
	for _, k := range tx.Keys(scope.Key(service, kindStack, "")) {
		st, _, err := storage.Read[stack](tx, k)
		if err != nil {
			return nil, err
		}

		if st.Name != except && slices.Contains(st.Imports, name) {
			out = append(out, st.Name)
		}
	}

	return out, nil
}

// event records a stack event.
func (s *Service) event(
	tx storage.WriteTx, scope request.Scope, st *stack, logical, physical, typ, status, reason string, p props,
) error {
	st.Events++
	e := stackEvent{
		Logical: logical, Physical: physical, Type: typ, Status: status, Reason: reason, Time: s.now(),
		ID: uuid.NewV4().String(),
	}
	if st.Op != nil {
		e.Token = st.Op.Token
	}

	if p != nil {
		if typ == mqBrokerType {
			p = mqEventProperties(p)
		}
		data, _ := json.Marshal(p)
		e.Properties = string(data)
	}

	return storage.Write(tx, eventKey(scope, st.ID, st.Events), e)
}

// stackEvent is a stack event's record.
type stackEvent struct {
	Time       time.Time
	ID         string
	Logical    string
	Physical   string `json:",omitempty"`
	Type       string
	Status     string
	Reason     string `json:",omitempty"`
	Properties string `json:",omitempty"`
	Token      string `json:",omitempty"`
}
