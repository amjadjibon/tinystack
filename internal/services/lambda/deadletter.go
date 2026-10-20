package lambda

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Only standard SQS queues and SNS topics are supported, not FIFO targets.
// https://docs.aws.amazon.com/lambda/latest/dg/invocation-async-retain-records.html#invocation-dlq
var deadLetterARN = regexp.MustCompile(`^arn:aws[a-zA-Z-]*:(sqs|sns):[a-z0-9-]+:\d{12}:[A-Za-z0-9_-]{1,256}$`)

func checkDeadLetterARN(arn string) error {
	if arn == "" {
		return nil
	}

	if !deadLetterARN.MatchString(arn) ||
		(strings.Contains(arn, ":sqs:") && len(arn[strings.LastIndex(arn, ":")+1:]) > 80) {
		return invalid("Invalid dead letter queue ARN: %s. A standard SQS queue or SNS topic ARN is required.", arn)
	}

	return nil
}

// sendDeadLetter sends the original event, with the documented attributes, independently of OnFailure delivery.
// AWS documents delivery to both targets, without a relative ordering guarantee:
// https://aws.amazon.com/blogs/compute/introducing-aws-lambda-destinations/
// Versions use the current $LATEST dead-letter configuration:
// https://docs.aws.amazon.com/lambda/latest/dg/invocation-async-retain-records.html
func (s *Service) sendDeadLetter(ctx context.Context, scope request.Scope, name, arn, id string,
	payload []byte, r *attemptResult,
) {
	var target string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		fn, _, readErr := loadFunction(tx, scope.Key("lambda", kindFunction, name))
		if fn != nil {
			target = fn.DeadLetterARN
		}

		return readErr
	})
	if target == "" && err == nil {
		return
	}

	if err == nil {
		err = s.deliver(ctx, target, arn, r.condition, payload, deadLetterAttributes(id, r))
	}

	s.deliveries.Add(delivery.ViaLambdaDeadLetter, arn, target, payload, err)
	if err != nil {
		s.logger.Warn("sending an async event to its dead-letter target failed", "function", arn, "target", target,
			"error", err)
	}
}

// deadLetterAttributes follows the HTTP status of Invoke (200 even for a function error), and limits the error
// message to the first 1 KB, without splitting a UTF-8 character.
// https://docs.aws.amazon.com/lambda/latest/dg/invocation-async-retain-records.html#invocation-dlq
func deadLetterAttributes(id string, r *attemptResult) map[string]any {
	code, message := http.StatusOK, string(r.result.Payload)
	if r.err != nil {
		e := awserror.From(r.err)
		code, message = e.StatusCode, e.Message
	} else {
		var response struct {
			ErrorMessage string `json:"errorMessage"`
		}

		if json.Unmarshal(r.result.Payload, &response) == nil && response.ErrorMessage != "" {
			message = response.ErrorMessage
		}
	}

	if len(message) > 1024 {
		message = message[:1024]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}

	const (
		dataType    = "DataType"
		stringValue = "StringValue"
	)
	return map[string]any{
		"RequestID":    map[string]any{dataType: "String", stringValue: id},
		"ErrorCode":    map[string]any{dataType: "Number", stringValue: strconv.Itoa(code)},
		"ErrorMessage": map[string]any{dataType: "String", stringValue: message},
	}
}
