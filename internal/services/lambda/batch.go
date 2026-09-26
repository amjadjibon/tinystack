package lambda

import (
	"bytes"
	"encoding/json"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
)

const (
	reportBatchItemFailures = "ReportBatchItemFailures"
	maxBatchingWindow       = 300
	maxFilters              = 5
)

// filterPatterns is the event filtering syntax, which has no wildcard.
var filterPatterns = pattern.Dialect{MaxNumeric: 5e9}

// batchOptions checks and stores the options that queue and stream mappings
// share: the batching window, filters, and batch item failure reporting.
func batchOptions(in *api.CreateEventSourceMappingRequest, mp *mapping) error {
	if window := value(in.MaximumBatchingWindowInSeconds); window < 0 || window > maxBatchingWindow {
		return invalid("MaximumBatchingWindowInSeconds must be between 0 and %d", maxBatchingWindow)
	}

	mp.BatchingWindow = value(in.MaximumBatchingWindowInSeconds)
	for _, t := range in.FunctionResponseTypes {
		if t != reportBatchItemFailures {
			return invalid("FunctionResponseTypes must be %s", reportBatchItemFailures)
		}

		mp.ReportFailures = true
	}

	if in.FilterCriteria == nil {
		return nil
	}

	if len(in.FilterCriteria.Filters) > maxFilters {
		return invalid("FilterCriteria can have at most %d filters", maxFilters)
	}

	for _, f := range in.FilterCriteria.Filters {
		if _, err := pattern.Compile(value(f.Pattern), filterPatterns); err != nil {
			return invalid("Invalid filter pattern definition. %s", err.Error())
		}

		mp.Filters = append(mp.Filters, value(f.Pattern))
	}

	return nil
}

// batchingWindow is how long a poller gathers records for one batch.
func (mp *mapping) batchingWindow() time.Duration {
	return time.Duration(mp.BatchingWindow) * time.Second
}

// filters compiles the mapping's filters, which were checked when it was
// created.
func (mp *mapping) filters() []*pattern.Pattern {
	var out []*pattern.Pattern
	for _, text := range mp.Filters {
		if p, err := pattern.Compile(text, filterPatterns); err == nil {
			out = append(out, p)
		}
	}

	return out
}

// keep reports whether a record, as the JSON document filters see, matches
// any filter; a mapping without filters keeps every record.
func keep(filters []*pattern.Pattern, document []byte) bool {
	if len(filters) == 0 {
		return true
	}

	event, ok := pattern.Flatten(document)
	if !ok {
		return false
	}

	for _, p := range filters {
		if p.Matches(event) {
			return true
		}
	}

	return false
}

// batchItemFailures reads the identifiers of the records a function reports
// failed with ReportBatchItemFailures. ok is false when the response fails the
// whole batch: it is not JSON, or an entry names no record of the batch. An
// empty or null response, or one without failures, succeeds.
func batchItemFailures(payload []byte, ids map[string]bool) (failed map[string]bool, ok bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, true
	}

	var response any
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, false
	}

	object, isObject := response.(map[string]any)
	if !isObject || object["batchItemFailures"] == nil {
		return nil, true
	}

	entries, isList := object["batchItemFailures"].([]any)
	if !isList {
		return nil, false
	}

	failed = map[string]bool{}
	for _, entry := range entries {
		item, isItem := entry.(map[string]any)
		if !isItem {
			return nil, false
		}

		id, isString := item["itemIdentifier"].(string)
		if !isString || !ids[id] {
			return nil, false
		}

		failed[id] = true
	}

	return failed, true
}
