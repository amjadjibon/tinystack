package secretsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	stagePending = "AWSPENDING"
	// rotationIndex lists the secrets with rotation enabled, as
	// "aws/secretsmanager/rotation/<account>/<region>/<name>", so the scheduler
	// reads no other secret. The secret record holds the next rotation time.
	rotationIndex   = "aws/secretsmanager/rotation/"
	maxRotationDays = 1000
	// minRotationPeriod is the shortest schedule Secrets Manager accepts.
	minRotationPeriod = 4 * time.Hour
)

// rotationSteps are the steps a rotation function runs, in order.
var rotationSteps = []string{"createSecret", "setSecret", "testSecret", "finishSecret"}

// rotationInterval is how often the scheduler looks for secrets due for rotation.
var rotationInterval = time.Second

var (
	duration    = regexp.MustCompile(`^([1-9]|1\d|2[0-4])h$`)
	functionARN = regexp.MustCompile(`^arn:aws:lambda:[a-z0-9-]+:\d{12}:function:[A-Za-z0-9_-]+(:[A-Za-z0-9$_-]+)?$`)
)

// rotation is a secret's rotation configuration and state. Times are Unix
// milliseconds.
type rotation struct {
	LambdaARN  string
	Expression string `json:",omitempty"`
	Duration   string `json:",omitempty"`
	Days       int64  `json:",omitempty"`
	// Next is when the scheduler rotates the secret next, or 0 without rules.
	Next        int64 `json:",omitempty"`
	LastRotated int64 `json:",omitempty"`
	Enabled     bool
}

func rotationKey(scope request.Scope, name string) string {
	return rotationIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

// rules validates RotationRules: AutomaticallyAfterDays (1 to 1000) or a
// ScheduleExpression that rotates at most every four hours, and an optional
// Duration of 1 to 24 hours.
func rules(in *api.RotationRulesType) (*rotation, *schedule.Schedule, error) {
	r := &rotation{Expression: value0(in.ScheduleExpression), Duration: value0(in.Duration)}
	if in.AutomaticallyAfterDays != nil {
		r.Days = *in.AutomaticallyAfterDays
	}

	switch {
	case in.AutomaticallyAfterDays != nil && in.ScheduleExpression != nil:
		return nil, nil, invalidParameter("You can't specify both AutomaticallyAfterDays and ScheduleExpression.")
	case in.AutomaticallyAfterDays == nil && in.ScheduleExpression == nil:
		return nil, nil, invalidParameter("RotationRules need AutomaticallyAfterDays or ScheduleExpression.")
	case in.AutomaticallyAfterDays != nil && (r.Days < 1 || r.Days > maxRotationDays):
		return nil, nil, invalidParameter("AutomaticallyAfterDays must be between 1 and %d.", maxRotationDays)
	case in.Duration != nil && !duration.MatchString(r.Duration):
		return nil, nil, invalidParameter("Duration must be 1h to 24h.")
	}

	if r.Expression == "" {
		return r, nil, nil
	}

	s, err := schedule.Parse(r.Expression)
	switch {
	case errors.Is(err, schedule.ErrUnsupported):
		return nil, nil, invalidParameter("TinyStack does not support L, W, or # in cron expressions.")
	case err != nil:
		return nil, nil, invalidParameter("The ScheduleExpression is not valid.")
	case strings.HasPrefix(r.Expression, "rate(") && s.Period() < minRotationPeriod:
		return nil, nil, invalidParameter("The rotation schedule must be at least 4 hours.")
	}

	return r, s, nil
}

// next is the first rotation time after now: AutomaticallyAfterDays after the
// last rotation (or now), or the schedule's next fire time. It is 0 when the
// schedule never fires again.
func (r *rotation) next(now time.Time) int64 {
	if r.Days > 0 {
		base := now
		if r.LastRotated > 0 {
			base = time.UnixMilli(r.LastRotated)
		}

		at := base.Add(time.Duration(r.Days) * day)
		if at.Before(now) {
			at = now
		}

		return at.UnixMilli()
	}

	s, err := schedule.Parse(r.Expression)
	if err != nil {
		return 0
	}

	at, ok := s.First(now)
	if !ok {
		return 0
	}

	return at.UnixMilli()
}

// index keeps the scheduler's entry for sec current.
func index(tx storage.WriteTx, scope request.Scope, sec *secret) {
	if sec.Rotation != nil && sec.Rotation.Enabled && sec.Rotation.Next > 0 {
		tx.Put(rotationKey(scope, sec.Name), []byte("{}"))
		return
	}

	tx.Delete(rotationKey(scope, sec.Name))
}

// pending returns the version labeled AWSPENDING that is not AWSCURRENT: a
// rotation that has not finished.
func (s *secret) pending() string {
	if id := s.Stages[stagePending]; id != "" && id != s.Stages[stageCurrent] {
		return id
	}

	return ""
}

// RotateSecret configures rotation (a Lambda function ARN and rules, kept from
// an earlier call when left out) and, unless RotateImmediately is false, starts
// a rotation: the function runs its createSecret, setSecret, testSecret, and
// finishSecret steps in the background, and the response carries the new
// version's ID. A rotation that has not finished blocks another.
func (s *Service) RotateSecret(ctx context.Context, in *api.RotateSecretRequest) (*api.RotateSecretResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkToken(in.ClientRequestToken); guardErr != nil {
		return nil, guardErr
	}

	if in.RotationLambdaARN != nil && !functionARN.MatchString(*in.RotationLambdaARN) {
		return nil, invalidParameter("RotationLambdaARN must be a Lambda function ARN.")
	}

	var configured *rotation
	if in.RotationRules != nil {
		if configured, _, err = rules(in.RotationRules); err != nil {
			return nil, err
		}
	}

	immediately := in.RotateImmediately == nil || *in.RotateImmediately
	token := value0(in.ClientRequestToken)
	if token == "" {
		token = newVersionID()
	}

	now := s.now()
	var out *api.RotateSecretResponse
	var sec *secret
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var findErr error
		if sec, findErr = findLive(tx, m.Scope, in.SecretId, now); findErr != nil {
			return findErr
		}

		if id := sec.pending(); id != "" && id != token {
			return invalidRequest("A previous rotation isn't complete. That rotation will be reattempted.")
		}

		r := sec.Rotation
		if r == nil {
			r = &rotation{}
		}

		if configured != nil {
			configured.LambdaARN, configured.LastRotated = r.LambdaARN, r.LastRotated
			r = configured
		}

		if in.RotationLambdaARN != nil {
			r.LambdaARN = *in.RotationLambdaARN
		}

		if r.LambdaARN == "" {
			return invalidRequest("No Lambda rotation function ARN is associated with this secret.")
		}

		r.Enabled = true
		if r.Days > 0 || r.Expression != "" {
			r.Next = r.next(now)
		}

		sec.Rotation = r
		index(tx, m.Scope, sec)
		out = &api.RotateSecretResponse{ARN: new(sec.arn(m.Scope)), Name: new(sec.Name)}
		if immediately {
			out.VersionId = &token
		}

		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})
	if err != nil {
		return nil, err
	}

	if immediately {
		s.spawn(func(ctx context.Context) { s.rotate(ctx, m.Scope, sec.Name, sec.Rotation.LambdaARN, token) })
	}

	return out, nil
}

// CancelRotateSecret turns rotation off. A rotation in progress keeps its
// AWSPENDING version, whose ID the response carries, as in Secrets Manager.
func (s *Service) CancelRotateSecret(
	ctx context.Context,
	in *api.CancelRotateSecretRequest,
) (*api.CancelRotateSecretResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.CancelRotateSecretResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, in.SecretId, s.now())
		if findErr != nil {
			return findErr
		}

		if sec.Rotation != nil {
			sec.Rotation.Enabled, sec.Rotation.Next = false, 0
		}

		index(tx, m.Scope, sec)
		out = &api.CancelRotateSecretResponse{
			ARN: new(sec.arn(m.Scope)), Name: new(sec.Name), VersionId: optional(sec.pending()),
		}

		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// rotate runs the rotation function's steps for version token of the secret
// name, acting in scope, and records the rotation once finishSecret has made
// token AWSCURRENT. A failed step ends the rotation, leaving token AWSPENDING
// for the next attempt; the delivery log records every step.
func (s *Service) rotate(ctx context.Context, scope request.Scope, name, function, token string) {
	var arn string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		sec, found, err := load(tx, scope, name)
		if err == nil && found {
			arn = sec.arn(scope)
		}

		return err
	})
	if err != nil || arn == "" {
		return
	}

	ctx = request.WithMetadata(
		ctx,
		request.Metadata{Scope: scope, RequestID: newVersionID(), Service: "secretsmanager"},
	)
	for _, step := range rotationSteps {
		payload, _ := json.Marshal(map[string]string{"Step": step, "SecretId": arn, "ClientRequestToken": token})
		out, invokeErr := s.functions.Invoke(
			ctx,
			&lambdaapi.InvocationRequest{FunctionName: &function, Payload: payload},
		)
		if invokeErr == nil && out.FunctionError != nil {
			invokeErr = fmt.Errorf("%s failed: %s", step, out.Payload)
		}

		s.deliveries.Add(delivery.ViaSecretsManager, arn, function, payload, invokeErr)
		if invokeErr != nil {
			return
		}
	}

	now := s.now()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, found, loadErr := load(tx, scope, name)
		if loadErr != nil || !found || sec.Stages[stageCurrent] != token || sec.Rotation == nil {
			if loadErr == nil && found {
				loadErr = fmt.Errorf("finishSecret did not make version %s AWSCURRENT", token)
			}

			return loadErr
		}

		sec.Rotation.LastRotated = now.UnixMilli()
		if sec.Rotation.Enabled && sec.Rotation.Days > 0 {
			sec.Rotation.Next = sec.Rotation.next(now)
		}

		index(tx, scope, sec)
		return storage.Write(tx, secretKey(scope, name), sec)
	})
	if err != nil && ctx.Err() == nil {
		s.deliveries.Add(delivery.ViaSecretsManager, arn, function, nil, err)
	}
}

// scheduler rotates due secrets every rotationInterval until Stop.
func (s *Service) scheduler(ctx context.Context) {
	ticker := time.NewTicker(rotationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := s.rotateDue(ctx, s.now()); err != nil && ctx.Err() == nil {
			s.logger.Warn("rotate Secrets Manager secrets", "error", err)
		}
	}
}

// due is a scheduled rotation to start.
type due struct {
	scope                 request.Scope
	name, function, token string
}

// rotateDue starts the rotation of every secret due at now. The transaction
// that claims a rotation moves the secret's next rotation time forward first,
// so a rotation starts at most once per schedule slot, even across a restart.
// A rotation that has not finished is reattempted with its AWSPENDING version.
func (s *Service) rotateDue(ctx context.Context, now time.Time) error {
	var runs []due
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		runs = nil
		for _, k := range tx.Keys(rotationIndex) {
			parts := strings.SplitN(strings.TrimPrefix(k, rotationIndex), "/", 3)
			if len(parts) != 3 {
				tx.Delete(k)
				continue
			}

			scope := request.Scope{AccountID: parts[0], Region: parts[1]}
			sec, found, err := load(tx, scope, parts[2])
			if err != nil {
				return err
			}

			if !found || sec.scheduled() || sec.Rotation == nil || !sec.Rotation.Enabled || sec.Rotation.Next == 0 {
				tx.Delete(k)
				continue
			}

			if sec.Rotation.Next > now.UnixMilli() {
				continue
			}

			token := sec.pending()
			if token == "" {
				token = newVersionID()
			}

			runs = append(runs, due{scope: scope, name: sec.Name, function: sec.Rotation.LambdaARN, token: token})

			sec.Rotation.Next = sec.Rotation.next(now)
			index(tx, scope, sec)
			if writeErr := storage.Write(tx, secretKey(scope, sec.Name), sec); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, r := range runs {
		s.spawn(func(ctx context.Context) { s.rotate(ctx, r.scope, r.name, r.function, r.token) })
	}

	return nil
}

// describeRotation adds a secret's rotation settings to its description.
func (s *secret) describeRotation(out *api.DescribeSecretResponse) {
	r := s.Rotation
	if r == nil {
		return
	}

	out.RotationEnabled = new(r.Enabled)
	out.RotationLambdaARN = optional(r.LambdaARN)
	if r.Days > 0 || r.Expression != "" {
		out.RotationRules = &api.RotationRulesType{
			Duration:           optional(r.Duration),
			ScheduleExpression: optional(r.Expression),
		}
		if r.Days > 0 {
			out.RotationRules.AutomaticallyAfterDays = new(r.Days)
		}
	}

	if r.LastRotated > 0 {
		out.LastRotatedDate = seconds(r.LastRotated)
	}

	if r.Enabled && r.Next > 0 {
		out.NextRotationDate = seconds(r.Next)
	}
}
