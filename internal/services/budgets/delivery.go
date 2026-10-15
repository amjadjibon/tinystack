package budgets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	jobPending       = "pending"
	jobIndex         = "aws/budgets/delivery/"
	deliveryAttempts = 4
	deliveryLease    = time.Minute
)

// deliveryJob retains the subscriber snapshot, stable logical event ID and final
// outcome. Only pending jobs appear in the partition-wide delivery index.
type deliveryJob struct {
	Subscriber                                          api.Subscriber
	ID, EventID, Account, Budget, Source, Subject, Body string
	Status, Error                                       string
	Attempts                                            int
	Next                                                int64
}

func jobPrefix(scope request.Scope, name string) string {
	return scope.Key(serviceName, "delivery", base64.RawURLEncoding.EncodeToString([]byte(name))+"/")
}

func jobKey(job *deliveryJob) string {
	return jobPrefix(request.Scope{AccountID: job.Account}, job.Budget) + job.ID
}

func alertMessage(scope request.Scope, b *api.Budget, n *api.Notification, limit *api.Spend,
	threshold *big.Rat, now time.Time, eventID string,
) (subject, body string) {
	name := *b.BudgetName
	budgeted := "unavailable"
	if limit != nil {
		budgeted = "$" + *limit.Amount
	}

	// SNS limits subjects to 100 bytes; keep the full name in the body.
	subject = "AWS Budgets: " + name + " has exceeded your alert threshold"
	for len(subject) > 100 {
		subject = string([]rune(subject)[:len([]rune(subject))-1])
	}

	subject = strings.NewReplacer("\n", " ", "\r", " ").Replace(subject)
	operator := map[string]string{greaterThan: ">", lessThan: "<", equalTo: "="}[*n.ComparisonOperator]
	start, end, _ := budgetPeriod(b, now)
	body = fmt.Sprintf("AWS Budget Notification %s\nAWS Account %s\n\n"+
		"Budget Name: %s\nBudget Type: Cost\nBudgeted Amount: %s\nAlert Type: ACTUAL\n"+
		"Alert Threshold: %s $%s\nACTUAL Amount: $%s\nBudget Period: %s to %s (UTC, end exclusive)\n"+
		"Event ID: %s\n\nTinyStack fixture-backed budget alert.\n",
		now.Format("January 02, 2006"), scope.AccountID, name, budgeted, operator,
		threshold.FloatString(14), *b.CalculatedSpend.ActualSpend.Amount, start.Format(time.RFC3339),
		end.Format(time.RFC3339), eventID)
	return subject, body
}

func (s *Service) deliverDue(ctx context.Context, now time.Time) error {
	var indexes []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error { indexes = tx.Keys(jobIndex); return nil }); err != nil {
		return err
	}

	for _, index := range indexes {
		var claimed *deliveryJob
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			k, ok := tx.Get(index)
			if !ok {
				return nil
			}

			job, ok, err := storage.Read[deliveryJob](tx, string(k))
			if err != nil {
				return err
			}

			if !ok {
				tx.Delete(index)
				return nil
			}

			if job.Next > now.UnixMilli() {
				return nil
			}

			if job.Attempts >= deliveryAttempts {
				job.Status, job.Error = "failed", "Delivery lease expired after the final attempt; outcome unknown."
				tx.Delete(index)
				return storage.Write(tx, string(k), job)
			}

			job.Attempts++
			job.Next = now.Add(deliveryLease).UnixMilli()
			if err := storage.Write(tx, string(k), job); err != nil {
				return err
			}

			claimed = job
			return nil
		})
		if err != nil {
			return err
		}

		if claimed == nil {
			continue
		}

		outcome := s.sendAlert(ctx, claimed)
		err = s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
			current, ok, readErr := storage.Read[deliveryJob](tx, jobKey(claimed))
			if readErr != nil {
				return readErr
			}

			// Reset/deletion or a newer lease must never be undone by an old attempt.
			if !ok || current.Attempts != claimed.Attempts {
				return nil
			}

			claimed.Status, claimed.Error = "delivered", ""
			if outcome != nil {
				claimed.Status, claimed.Error = jobPending, outcome.Error()
				claimed.Next = now.Add(time.Second << (claimed.Attempts - 1)).UnixMilli()
				if claimed.Attempts >= deliveryAttempts {
					claimed.Status = "failed"
				}
			}

			if claimed.Status != jobPending {
				tx.Delete(index)
			}

			return storage.Write(tx, jobKey(claimed), claimed)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) sendAlert(ctx context.Context, job *deliveryJob) error {
	target := common.Value(job.Subscriber.Address)
	var err error
	var payload []byte
	if common.Value(job.Subscriber.SubscriptionType) == "EMAIL" {
		payload, err = json.Marshal(delivery.Message{
			ID: job.EventID, Channel: "email", Service: serviceName,
			Source: job.Source, To: target, Subject: job.Subject, Body: job.Body, Account: job.Account,
		})
	} else {
		payload = []byte(job.Body)
		topic, parseErr := awsarn.Parse(target)
		switch {
		case parseErr != nil || topic.Partition != "aws" || topic.Service != "sns" || topic.Region == "" ||
			topic.Resource == "" || strings.HasSuffix(topic.Resource, ".fifo"):
			err = fmt.Errorf("budget alerts require a standard SNS topic ARN")
		case topic.AccountID != job.Account:
			err = fmt.Errorf("budget alerts require an SNS topic in account %s", job.Account)
		default:
			callCtx := request.WithMetadata(ctx, request.Metadata{Scope: request.Scope{
				AccountID: topic.AccountID, Region: topic.Region,
			}, RequestID: job.EventID, Service: "sns"})
			_, err = s.caller.Call(callCtx, "sns", "Publish", map[string]any{
				"TopicArn": target, "Subject": job.Subject, "Message": job.Body,
			})
		}
	}

	s.deliveries.Add(delivery.ViaBudgets, job.Source, target, payload, err)
	return err
}
