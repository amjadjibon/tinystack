// Package delivery records the messages TinyStack's services hand to each
// other (SNS fan-out, EventBridge targets, asynchronous Lambda invocations, and
// event source mappings) so that a failed delivery, which AWS never reports to
// the sender, is still visible. It also records the messages Cognito would send
// to users, which TinyStack never sends.
package delivery

import (
	"time"

	"github.com/amjadjibon/tinystack/internal/recent"
)

// maxRecords is how many recent deliveries the log keeps.
const maxRecords = 256

// Delivery kinds.
const (
	ViaSNS                = "sns"
	ViaEventBridge        = "eventbridge"
	ViaLambdaAsync        = "lambda-async"
	ViaEventSourceMapping = "event-source-mapping"
	ViaS3                 = "s3"
	ViaSecretsManager     = "secretsmanager"
	// ViaFirehose records the objects delivery streams write.
	ViaFirehose = "firehose"
	// ViaCognito records the email and SMS messages a user pool would send,
	// such as confirmation codes and temporary passwords.
	ViaCognito = "cognito"
)

// Record is one delivery. Source is the topic, rule, queue, or bucket ARN it came
// from, empty for asynchronous invocations; Payload keeps the first
// recent.MaxText bytes and Size counts them all.
type Record struct {
	Time    time.Time `json:"time"`
	Via     string    `json:"via"`
	Source  string    `json:"source,omitempty"`
	Target  string    `json:"target"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
	Payload string    `json:"payload"`
	Size    int       `json:"size"`
}

// Log keeps recent deliveries and serves them as {"deliveries": [...]}.
type Log struct {
	*recent.Log[Record]
}

func New() *Log {
	return &Log{recent.New[Record](maxRecords, "deliveries")}
}

// Add records a delivery of payload from source to target; err is nil when it
// succeeded.
func (l *Log) Add(via, source, target string, payload []byte, err error) {
	r := Record{
		Time:    time.Now().UTC(),
		Via:     via,
		Source:  source,
		Target:  target,
		Status:  "delivered",
		Payload: recent.Text(payload),
		Size:    len(payload),
	}

	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	}

	l.Log.Add(r)
}
