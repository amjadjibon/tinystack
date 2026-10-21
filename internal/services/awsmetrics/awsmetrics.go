// Package awsmetrics is how services report their built-in AWS/* CloudWatch
// metrics. A service holds a Meter, which does nothing until the application
// hands it CloudWatch's Sink, so services never import CloudWatch.
//
// Report after the transaction that caused the metric has committed, never
// inside one. Publish must stay cheap: it only records the datum in memory.
package awsmetrics

import "github.com/amjadjibon/tinystack/internal/aws/request"

// Units CloudWatch accepts for the built-in metrics.
const (
	Count        = "Count"
	Bytes        = "Bytes"
	Milliseconds = "Milliseconds"
	Seconds      = "Seconds"
	None         = "None"
)

// Dimension is one metric dimension.
type Dimension struct {
	Name  string
	Value string
}

// Datum is one observation of a metric.
type Datum struct {
	Name       string
	Unit       string
	Dimensions []Dimension
	Value      float64
}

// Sink receives the observations of one account and Region under an AWS/*
// namespace.
type Sink interface {
	Publish(scope request.Scope, namespace string, data ...Datum)
}

// Meter forwards to a Sink and discards observations until Use sets one. Use
// it before serving requests.
type Meter struct{ sink Sink }

func (m *Meter) Use(sink Sink) { m.sink = sink }

func (m *Meter) Publish(scope request.Scope, namespace string, data ...Datum) {
	if m.sink != nil {
		m.sink.Publish(scope, namespace, data...)
	}
}
