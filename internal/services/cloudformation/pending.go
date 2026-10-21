package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func stepResourceType(st *stack, next *step) string {
	if next.kind == stepReplace {
		return st.Resources[next.logical].Type
	}
	return next.typ
}

// pendingStep is a claimed step waiting for an external completion. Its inputs
// survive visits and restarts; only dispatch and completion run outside storage
// transactions. A provider can receive a retried request with the same RequestId.
type pendingStep struct {
	Deadline     time.Time
	Properties   props
	Declaration  *resourceDecl
	TargetType   string
	Kind         string
	Type         string
	Physical     string
	ChildName    string
	RequestID    string
	ResponseID   string
	Logical      string
	Dependencies []string
	Noop         bool
	NoEcho       bool
	Sent         bool
}

func (p *pendingStep) step() *step {
	if p.Declaration != nil {
		p.Declaration.deps = p.Dependencies
	}

	return &step{kind: p.Kind, logical: p.Logical, typ: p.TargetType, props: p.Properties, decl: p.Declaration}
}

func (s *Service) beginPending(ctx context.Context, ref stackRef, st *stack, next *step) (bool, error) {
	p := &pendingStep{
		Kind:        next.kind,
		Logical:     next.logical,
		Type:        stepResourceType(st, next),
		TargetType:  next.typ,
		Properties:  next.props,
		Declaration: next.decl,
		RequestID:   uuid.NewV4().String(),
		ResponseID:  uuid.NewV4().String(),
		Deadline:    s.now().Add(s.CustomResourceTimeout),
	}

	if next.decl != nil {
		p.Dependencies = next.decl.deps
	}

	r := st.Resources[next.logical]
	if next.kind == stepDiscard {
		r = st.Replaced[0].Resource
	}

	if r != nil {
		p.Physical = r.Physical
	}

	if p.Type == stackType && (next.kind == stepCreate || next.kind == stepSwap) {
		c := &client{stack: st.Name, logical: next.logical}
		p.ChildName = c.generate(128)
	}

	err := s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
		cur.Op.Pending = p
		if isCustomResource(p.Type) {
			return storage.Write(tx, responseKey(p.ResponseID), &customWait{
				Scope: ref.scope, StackID: st.ID,
				RequestID: p.RequestID, Logical: p.Logical, Deadline: p.Deadline,
			})
		}

		return nil
	})
	return err == nil, err
}

func (s *Service) resumePending(ctx context.Context, ref stackRef, st *stack) (bool, error) {
	p := st.Op.Pending
	if !p.Sent {
		if p.Kind == stepDiscard {
			live, old := st.Resources[p.Logical], st.Replaced[0].Resource
			if live != nil && live.Type == old.Type && live.Physical == old.Physical {
				return true, s.completePending(ctx, ref, p, "", nil, nil)
			}
		}

		physical, err := s.dispatchPending(ctx, ref.scope, st, p)
		if err != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}

		if err != nil {
			return true, s.completePending(ctx, ref, p, "", nil, err)
		}

		return true, s.commit(ctx, ref, func(_ storage.WriteTx, cur *stack) error {
			cur.Op.Pending.Sent = true
			cur.Op.Pending.Noop = p.Noop
			cur.Op.Pending.Physical = physical
			// Keep the child identity even if it fails, so rollback can delete it.
			if p.Type == stackType && p.Kind == stepCreate {
				cur.Resources[p.Logical].Physical = physical
			}

			return nil
		})
	}

	if p.Type == stackType {
		return s.pollNested(ctx, ref, p)
	}

	var wait *customWait
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		wait, _, err = storage.Read[customWait](tx, responseKey(p.ResponseID))
		return err
	})
	if err != nil {
		return false, err
	}

	if wait != nil && wait.Response != nil {
		response := wait.Response
		if response.Status == customStatusFailed {
			return true, s.completePending(
				ctx,
				ref,
				p,
				response.PhysicalResourceID,
				nil,
				validation("%s", response.Reason),
			)
		}

		p.NoEcho = response.NoEcho
		return true, s.completePending(ctx, ref, p, response.PhysicalResourceID, response.Data, nil)
	}

	if !s.now().Before(p.Deadline) {
		return true, s.completePending(ctx, ref, p, "", nil, validation("Custom resource response timed out"))
	}

	return false, nil
}

func (s *Service) dispatchPending(
	ctx context.Context, scope request.Scope, st *stack, p *pendingStep,
) (string, error) {
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Endpoint: st.Endpoint})
	if p.Type == stackType {
		return s.dispatchNested(ctx, st, p)
	}

	return s.dispatchCustom(ctx, st, p)
}

func (s *Service) completePending(
	ctx context.Context, ref stackRef, p *pendingStep, physical string, attrs map[string]any, failure error,
) error {
	return s.commit(ctx, ref, func(tx storage.WriteTx, cur *stack) error {
		if failure != nil && physical != "" && p.Kind == stepCreate {
			cur.Resources[p.Logical].Physical = physical
			cur.Resources[p.Logical].Properties = p.Properties
		}

		if failure == nil && (p.Kind == stepCreate || p.Kind == stepUpdate || p.Kind == stepSwap) {
			cur.Resources[p.Logical].NoEcho = p.NoEcho
		}

		cur.Op.Pending = nil
		if isCustomResource(p.Type) {
			tx.Delete(responseKey(p.ResponseID))
		}

		return s.record(tx, ref.scope, cur, p.step(), physical, attrs, failure)
	})
}

func (s *Service) pollNested(ctx context.Context, ref stackRef, p *pendingStep) (bool, error) {
	child, err := s.current(ctx, ref.scope, p.Physical)
	if err != nil {
		return true, s.completePending(ctx, ref, p, "", nil, err)
	}

	if child.Op != nil || strings.HasSuffix(child.Status, "_IN_PROGRESS") {
		return false, nil
	}

	deleting := p.Kind == stepRemove || p.Kind == stepDiscard || p.Kind == stepReplace
	success := p.Noop || child.Status == statusCreateComplete || child.Status == statusUpdateComplete
	if deleting {
		success = child.Status == statusDeleteComplete
	}

	if !success {
		reason := child.Reason
		if reason == "" {
			_ = s.store.View(ctx, func(tx storage.ReadTx) error {
				for _, key := range tx.Keys(ref.scope.Key(service, kindEvent, stackUUID(child.ID)+"/")) {
					event, _, err := storage.Read[stackEvent](tx, key)
					if err != nil {
						return err
					}

					if strings.HasSuffix(event.Status, "_FAILED") && event.Reason != "" {
						reason = event.Reason
					}
				}

				return nil
			})
		}

		if reason == "" {
			reason = child.Status
		}

		return true, s.completePending(ctx, ref, p, child.ID, nil, validation("Nested stack %s: %s", child.ID, reason))
	}

	attrs := map[string]any{}
	for _, o := range child.Outputs {
		attrs["Outputs."+o.Key] = o.Value
	}

	return true, s.completePending(ctx, ref, p, child.ID, attrs, nil)
}

// responseKey is deliberately partition-wide: the unguessable response URL is
// a capability, usable without AWS credentials by a custom resource provider.
func responseKey(id string) string { return "aws/cloudformation/response/" + id }

type customWait struct {
	Deadline  time.Time
	Response  *CustomResourceResponse
	Scope     request.Scope
	StackID   string
	RequestID string
	Logical   string
}

// CustomResourceResponse is the provider response uploaded to ResponseURL.
type CustomResourceResponse struct {
	Data               map[string]any
	Status             string
	Reason             string
	PhysicalResourceID string `json:"PhysicalResourceId"`
	StackID            string `json:"StackId"`
	RequestID          string `json:"RequestId"`
	LogicalResourceID  string `json:"LogicalResourceId"`
	NoEcho             bool
}

// RespondCustomResource validates and records a response without advancing a
// stack in the HTTP request. Matching retries are idempotent; conflicting ones fail.
func (s *Service) RespondCustomResource(ctx context.Context, id string, body []byte) error {
	var response CustomResourceResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return validation("Invalid custom resource response JSON")
	}

	if response.Status != "SUCCESS" && response.Status != customStatusFailed {
		return validation("Custom resource response Status must be SUCCESS or FAILED")
	}

	if response.PhysicalResourceID == "" {
		return validation("PhysicalResourceId is required")
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		wait, found, err := storage.Read[customWait](tx, responseKey(id))
		if err != nil {
			return err
		}

		if !found || !s.now().Before(wait.Deadline) {
			return validation("Unknown or expired custom resource response URL")
		}

		if response.StackID != wait.StackID || response.RequestID != wait.RequestID ||
			response.LogicalResourceID != wait.Logical {
			return validation("Custom resource response identity does not match request")
		}

		if wait.Response != nil {
			old, _ := json.Marshal(wait.Response)
			current, _ := json.Marshal(&response)
			if !bytes.Equal(old, current) {
				return validation("Custom resource response was already received")
			}

			return nil
		}

		wait.Response = &response
		return storage.Write(tx, responseKey(id), wait)
	})
	if err == nil {
		s.poke()
	}

	return err
}
