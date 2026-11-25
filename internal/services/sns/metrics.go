package sns

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/SNS metrics a topic reports, with the dimension TopicName.
const (
	metricNamespace = "AWS/SNS"

	metricPublished            = "NumberOfMessagesPublished"
	metricPublishSize          = "PublishSize"
	metricDelivered            = "NumberOfNotificationsDelivered"
	metricFailed               = "NumberOfNotificationsFailed"
	metricFilteredOut          = "NumberOfNotificationsFilteredOut"
	metricFilteredOutNoAttrs   = "NumberOfNotificationsFilteredOut-NoMessageAttributes"
	metricRedrivenToDlq        = "NumberOfNotificationsRedrivenToDlq"
	metricFailedToRedriveToDlq = "NumberOfNotificationsFailedToRedriveToDlq"
)

// UseMetrics has the service report its AWS/SNS metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

func (s *Service) publishDatum(topicARN, name, unit string, value float64) {
	parts := strings.Split(topicARN, ":")
	s.metrics.Publish(request.Scope{AccountID: parts[4], Region: parts[3]}, metricNamespace, awsmetrics.Datum{
		Name: name, Unit: unit, Value: value,
		Dimensions: []awsmetrics.Dimension{{Name: "TopicName", Value: parts[5]}},
	})
}

// publishMetric reports one count for the topic named by topicARN.
func (s *Service) publishMetric(topicARN, name string) {
	s.publishDatum(topicARN, name, awsmetrics.Count, 1)
}

// publishSizeMetric reports PublishSize in bytes for a non-deduplicated publish.
func (s *Service) publishSizeMetric(topicARN string, size int) {
	s.publishDatum(topicARN, metricPublishSize, awsmetrics.Bytes, float64(size))
}

// notified reports the outcome of delivering a notification of the topic to one subscriber.
func (s *Service) notified(topicARN string, err error) {
	if err != nil {
		s.publishMetric(topicARN, metricFailed)
		return
	}

	s.publishMetric(topicARN, metricDelivered)
}

// filteredOut reports a notification dropped by a subscription's filter policy.
func (s *Service) filteredOut(topicARN string, noMessageAttributes bool) {
	s.publishMetric(topicARN, metricFilteredOut)
	if noMessageAttributes {
		s.publishMetric(topicARN, metricFilteredOutNoAttrs)
	}
}

// redriven reports the outcome of delivering a failed notification to a subscription's DLQ.
func (s *Service) redriven(topicARN string, err error) {
	if err != nil {
		s.publishMetric(topicARN, metricFailedToRedriveToDlq)
		return
	}

	s.publishMetric(topicARN, metricRedrivenToDlq)
}
