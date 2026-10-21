package sns

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/SNS metrics a topic reports, with the dimension TopicName.
const (
	metricNamespace = "AWS/SNS"

	metricPublished = "NumberOfMessagesPublished"
	metricDelivered = "NumberOfNotificationsDelivered"
	metricFailed    = "NumberOfNotificationsFailed"
)

// UseMetrics has the service report its AWS/SNS metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// publishMetric reports one count for the topic named by topicARN.
func (s *Service) publishMetric(topicARN, name string) {
	parts := strings.Split(topicARN, ":")
	s.metrics.Publish(request.Scope{AccountID: parts[4], Region: parts[3]}, metricNamespace, awsmetrics.Datum{
		Name: name, Unit: awsmetrics.Count, Value: 1,
		Dimensions: []awsmetrics.Dimension{{Name: "TopicName", Value: parts[5]}},
	})
}

// notified reports the outcome of delivering a notification of the topic to one subscriber.
func (s *Service) notified(topicARN string, err error) {
	if err != nil {
		s.publishMetric(topicARN, metricFailed)
		return
	}

	s.publishMetric(topicARN, metricDelivered)
}
