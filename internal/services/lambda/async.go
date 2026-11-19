package lambda

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const asyncIndex = "aws/lambda/index/"

type asyncQueueEntry struct {
	Due time.Time
	Key string
}

type asyncEvent struct {
	Queued      time.Time
	Due         time.Time
	Finished    time.Time
	Config      invokeConfig
	Payload     []byte
	Failure     *awserror.Error
	Name        string
	Qualifier   string
	ID          string
	Version     string
	Condition   string
	DeadLetter  string
	Destination string
	ObjectKey   string
	Response    Result
	Attempts    int
	Claimed     bool
}

func (s *Service) enqueueAsync(ctx context.Context, scope request.Scope, name, qualifier string, payload []byte) error {
	s.asyncMu.RLock()
	defer s.asyncMu.RUnlock()
	if s.asyncStopping || s.async.Context().Err() != nil {
		return awserror.New("ServiceException", "The service is shutting down")
	}

	event := asyncEvent{ID: uuid.NewV4().String(), Name: name, Qualifier: qualifier, Payload: payload, Queued: s.now()}
	index := asyncIndex + scope.AccountID + "/" + scope.Region + "/async/" + event.ID
	key := scope.Key("lambda", "async", event.ID)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, err := resolve(tx, scope, name, qualifier); err != nil {
			return err
		}

		if _, err := readJSON(tx, invokeConfigKey(scope, name, qualifier), &event.Config); err != nil {
			return err
		}

		return saveAsyncEvent(tx, index, key, &event)
	})
	if err != nil {
		return err
	}

	s.publish(scope, name, qualifier, counter(metricAsyncReceived, 1))
	s.wakeAsync()
	return nil
}

func (s *Service) wakeAsync() {
	select {
	case s.asyncWake <- struct{}{}:
	default:
	}
}

func (s *Service) startAsync(group *background.Group) {
	group.Go(func(ctx context.Context) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-s.asyncWake:
			}

			s.pollAsync(ctx, group)
		}
	})
}

func (s *Service) pollAsync(ctx context.Context, group *background.Group) {
	var keys []string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		now := s.now()
		for _, index := range tx.Keys(asyncIndex) {
			var entry asyncQueueEntry
			if _, err := readJSON(tx, index, &entry); err != nil {
				return err
			}

			if !now.Before(entry.Due) {
				keys = append(keys, index)
			}
		}

		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("scanning async Lambda queue", "error", err)
		}

		return
	}

	for _, index := range keys {
		if _, active := s.asyncActive.LoadOrStore(index, struct{}{}); active {
			continue
		}

		if !group.Go(func(ctx context.Context) {
			defer s.asyncActive.Delete(index)
			if err := s.processAsync(ctx, index); err != nil && ctx.Err() == nil {
				s.logger.Warn("processing async Lambda event", "index", index, "error", err)
			}
		}) {
			s.asyncActive.Delete(index)
			return
		}
	}
}

// A claim counts the attempt before running user code. A claim left behind by shutdown or process exit
// becomes a failed attempt when its record is picked up again.
func (s *Service) processAsync(ctx context.Context, index string) error {
	var event asyncEvent
	var key string
	var scope request.Scope
	var invoke, dropped, recovered bool
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		entry, found, readErr := storage.Read[asyncQueueEntry](tx, index)
		if readErr != nil || !found {
			return readErr
		}

		now := s.now()
		if now.Before(entry.Due) {
			return nil
		}

		key = entry.Key
		account, region, _, _ := storage.Scoped(key, "lambda")
		scope = request.Scope{AccountID: account, Region: region}
		found, err := readJSON(tx, key, &event)
		if err != nil || !found {
			return err
		}

		switch {
		case event.Condition != "":
			return nil
		case event.Claimed:
			recovered, event.Claimed = true, false
			event.Failure = &awserror.Error{
				Code:       "ServiceException",
				Message:    "Invocation interrupted",
				StatusCode: 500,
			}
			event.Response, event.Version = Result{}, ""
			dropped = s.finishAsyncAttempt(tx, scope, &event)
		case !now.Before(event.Queued.Add(event.Config.maxAge())):
			event.Failure = &awserror.Error{Code: "ServiceException", Message: "Event age exceeded", StatusCode: 500}
			s.finishAsync(tx, scope, &event, conditionEventAge)
			dropped = true
		default:
			event.Claimed, invoke = true, true
			event.Attempts++
		}

		return saveAsyncEvent(tx, index, key, &event)
	})
	if err != nil {
		return err
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, RequestID: event.ID})
	arn := qualifiedARN(scope, event.Name, event.Qualifier)
	if recovered {
		s.deliveries.Add(delivery.ViaLambdaAsync, "", arn, event.Payload, event.Failure)
	}

	if invoke {
		if dropped, err = s.invokeQueued(ctx, scope, arn, index, key, &event); err != nil {
			return err
		}
	}

	if dropped {
		s.publish(scope, event.Name, event.Qualifier, counter(metricAsyncDropped, 1))
	}

	if event.Condition != "" {
		return s.deliverQueued(ctx, scope, arn, index, key, &event)
	}

	return nil
}

func (s *Service) invokeQueued(
	ctx context.Context,
	scope request.Scope,
	arn, index, key string,
	event *asyncEvent,
) (bool, error) {
	ctx = context.WithValue(ctx, asyncArrivalKey{}, event.Queued)
	result, _, version, err := s.run(ctx, scope, event.Name, event.Qualifier, event.Payload)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	event.Claimed, event.Response, event.Version, event.Failure = false, result, version, nil
	event.Response.Log = ""
	failure := invokeError(result, err)
	if failure != nil {
		s.logger.Warn("async lambda invocation failed", "function", arn, "attempt", event.Attempts, "error", failure)
		failure = fmt.Errorf("attempt %d of %d: %w", event.Attempts, event.Config.attempts(), failure)
	}

	if err != nil {
		event.Failure = awserror.From(err)
	}

	var dropped bool
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		dropped = s.finishAsyncAttempt(tx, scope, event)
		return saveAsyncEvent(tx, index, key, event)
	})
	if err == nil {
		s.deliveries.Add(delivery.ViaLambdaAsync, "", arn, event.Payload, failure)
	}

	return dropped, err
}

func (s *Service) finishAsyncAttempt(tx storage.ReadTx, scope request.Scope, event *asyncEvent) bool {
	switch {
	case event.Failure == nil && !event.Response.Error:
		s.finishAsync(tx, scope, event, conditionSuccess)
	case event.Attempts >= event.Config.attempts():
		s.finishAsync(tx, scope, event, conditionRetries)
		return true
	default:
		event.Due = s.now().Add(retryDelay * time.Duration(event.Attempts))
		if expires := event.Queued.Add(event.Config.maxAge()); expires.Before(event.Due) {
			event.Due = expires
		}
	}

	return false
}

func saveAsyncEvent(tx storage.WriteTx, index, key string, event *asyncEvent) error {
	if err := storage.Write(tx, index, &asyncQueueEntry{Key: key, Due: event.Due}); err != nil {
		return err
	}

	return storage.Write(tx, key, event)
}

func (s *Service) finishAsync(tx storage.ReadTx, scope request.Scope, event *asyncEvent, condition string) {
	event.Condition, event.Finished = condition, s.now()
	if condition == conditionSuccess {
		event.Destination = event.Config.OnSuccess
		return
	}

	event.Destination = event.Config.OnFailure
	// Published versions also use the current $LATEST dead-letter configuration.
	fn, _, err := loadFunction(tx, scope.Key("lambda", kindFunction, event.Name))
	if err == nil && fn != nil {
		event.DeadLetter = fn.DeadLetterARN
	}

	if validS3Destination(event.Destination) {
		event.ObjectKey = s3RecordKey("aws/lambda/async/"+event.Name, event.Finished)
	}
}

func (s *Service) deliverQueued(
	ctx context.Context,
	scope request.Scope,
	arn, index, key string,
	event *asyncEvent,
) error {
	outcome := attemptResult{
		version: event.Version, condition: event.Condition, attempts: event.Attempts, result: event.Response,
	}
	if event.Failure != nil {
		outcome.err = event.Failure
	}

	if target := event.DeadLetter; target != "" {
		err := s.deliver(ctx, target, arn, event.Condition, event.Payload, deadLetterAttributes(event.ID, &outcome), "")
		if ctx.Err() != nil {
			return ctx.Err()
		}

		s.deliveries.Add(delivery.ViaLambdaDeadLetter, arn, target, event.Payload, err)
		s.reportDeliveryFailure(scope, event, target, metricDeadLetterErrors, err)
		event.DeadLetter = ""
		if err := s.store.Update(
			ctx,
			func(tx storage.WriteTx) error { return storage.Write(tx, key, event) },
		); err != nil {
			return err
		}
	}

	if target := event.Destination; target != "" {
		err := s.sendResult(ctx, arn, target, event.ID, event.ObjectKey, event.Finished, event.Payload, &outcome)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		s.reportDeliveryFailure(scope, event, target, metricDestinationFailures, err)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(index)
		tx.Delete(key)
		return nil
	})
}

func (s *Service) reportDeliveryFailure(scope request.Scope, event *asyncEvent, target, metric string, err error) {
	if err != nil {
		s.logger.Warn("async Lambda delivery failed", "function", event.Name, "target", target, "error", err)
		s.publish(scope, event.Name, event.Qualifier, counter(metric, 1))
	}
}

func (s *Service) resetAsync(ctx context.Context) {
	// Wait outside the lock so a stopping worker can finish an in-process destination call.
	s.asyncMu.RLock()
	group := s.async
	s.asyncMu.RUnlock()
	group.Stop()
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, index := range tx.Keys(asyncIndex) {
			entry, _, err := storage.Read[asyncQueueEntry](tx, index)
			if err != nil {
				return err
			}

			tx.Delete(entry.Key)
			tx.Delete(index)
		}

		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("clearing async Lambda events", "error", err)
	}

	if !s.asyncStopping {
		s.async = background.New()
		s.startAsync(s.async)
	}
}
