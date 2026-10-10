package cloudtrail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxHistory = 1000

type storedEvent struct {
	Event    api.Event
	Sequence uint64
}

type history struct {
	Events   []storedEvent
	Sequence uint64
}

type cursor struct {
	Filter string
	Before uint64
}

// Record runs only for external gateway operations. Parameters and response bodies
// are deliberately absent, including for operations marked sensitive by Smithy.
func (s *Service) Record(ctx context.Context, in *catalog.ManagementEvent) error {
	m := in.Metadata
	if m.Service == "" || m.Operation == "" {
		return nil
	}

	readOnly := false
	for _, prefix := range []string{"Get", "List", "Describe", "Lookup", "Head", "BatchGet", "Scan", "Query"} {
		if strings.HasPrefix(m.Operation, prefix) {
			readOnly = true
			break
		}
	}

	readonly := strconv.FormatBool(readOnly)
	source := m.Service + ".amazonaws.com"
	eventID := common.RandomID(32)
	document := map[string]any{
		"eventVersion": "1.09", "eventID": eventID, "eventName": m.Operation, "eventSource": source,
		"eventTime": in.Time.UTC().Format(time.RFC3339), "awsRegion": m.Region, "sourceIPAddress": in.SourceIPAddress,
		"userAgent": in.UserAgent, "readOnly": readOnly, "eventType": "AwsApiCall", "managementEvent": true,
		"recipientAccountId": m.AccountID, "requestID": m.RequestID,
		"userIdentity": map[string]any{
			"type": "IAMUser", "accountId": m.AccountID, "accessKeyId": m.AccessKey,
			"principalId": m.AccessKey, "userName": m.AccessKey,
		},
	}

	if in.ErrorCode != "" {
		document["errorCode"] = in.ErrorCode
		document["errorMessage"] = in.ErrorMessage
	}

	body, err := json.Marshal(document)
	if err != nil {
		return err
	}

	resources := []api.Resource{}
	for _, r := range in.Resources {
		resources = append(resources, api.Resource{ResourceType: new(r.Type), ResourceName: new(r.Name)})
	}

	event := api.Event{
		EventId: new(eventID), EventName: new(m.Operation), EventSource: new(source), Username: new(m.AccessKey),
		AccessKeyId: new(m.AccessKey), EventTime: new(float64(in.Time.UnixNano()) / 1e9), ReadOnly: new(readonly),
		CloudTrailEvent: new(string(body)), Resources: resources,
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := m.Key(serviceName, "history", "events")
		h, found, e := storage.Read[history](tx, key)
		if e != nil {
			return e
		}

		if !found {
			h = &history{}
		}

		h.Sequence++
		h.Events = append(h.Events, storedEvent{Sequence: h.Sequence, Event: event})
		cutoff := float64(time.Now().Add(-90 * 24 * time.Hour).Unix())
		start := 0
		for start < len(h.Events) && common.Value(h.Events[start].Event.EventTime) < cutoff {
			start++
		}

		if len(h.Events)-start > maxHistory {
			start = len(h.Events) - maxHistory
		}

		h.Events = h.Events[start:]
		return storage.Write(tx, key, h)
	})
}

func matches(e *api.Event, a api.LookupAttribute) bool {
	value := common.Value(a.AttributeValue)
	switch common.Value(a.AttributeKey) {
	case "EventId":
		return common.Value(e.EventId) == value
	case "EventName":
		return common.Value(e.EventName) == value
	case "EventSource":
		return common.Value(e.EventSource) == value
	case "Username":
		return common.Value(e.Username) == value
	case attributeReadOnly:
		return common.Value(e.ReadOnly) == value
	case "AccessKeyId":
		return common.Value(e.AccessKeyId) == value
	case attributeResourceName, attributeResourceType:
		for _, r := range e.Resources {
			if (common.Value(a.AttributeKey) == attributeResourceName && common.Value(r.ResourceName) == value) ||
				(common.Value(a.AttributeKey) == attributeResourceType && common.Value(r.ResourceType) == value) {
				return true
			}
		}
	}

	return false
}

func (s *Service) LookupEvents(ctx context.Context, in *api.LookupEventsRequest) (*api.LookupEventsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := int(common.Value(common.OrElse(in.MaxResults, new(int64(50)))))
	if limit < 1 || limit > 50 {
		return nil, awserror.New("InvalidMaxResultsException", "MaxResults must be 1 to 50")
	}

	if len(in.LookupAttributes) > 1 {
		return nil, awserror.New("InvalidLookupAttributesException", "Only one lookup attribute")
	}

	for _, a := range in.LookupAttributes {
		switch common.Value(a.AttributeKey) {
		case "EventId",
			"EventName",
			"EventSource",
			"Username",
			attributeReadOnly,
			"AccessKeyId",
			attributeResourceName,
			attributeResourceType:
			if common.Value(a.AttributeValue) == "" {
				return nil, invalid("AttributeValue must not be empty")
			}

		default:
			return nil, awserror.New("InvalidLookupAttributesException", "Unsupported lookup attribute")
		}
	}

	if in.StartTime != nil && in.EndTime != nil && *in.StartTime > *in.EndTime {
		return nil, awserror.New("InvalidTimeRangeException", "StartTime exceeds EndTime")
	}

	filters := struct {
		Scope      request.Scope
		Start, End *float64
		Attributes []api.LookupAttribute
	}{
		scope, in.StartTime, in.EndTime, in.LookupAttributes,
	}

	encoded, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}

	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	page := cursor{Filter: fingerprint}
	if in.NextToken != nil {
		b, e := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if e != nil || json.Unmarshal(b, &page) != nil || page.Filter != fingerprint || page.Before == 0 {
			return nil, awserror.New("InvalidNextTokenException", "Invalid token or changed lookup filters")
		}
	}

	out := &api.LookupEventsResponse{Events: []api.Event{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		h, found, e := storage.Read[history](tx, scope.Key(serviceName, "history", "events"))
		if e != nil || !found {
			return e
		}

		var last uint64
		for i := len(h.Events) - 1; i >= 0; i-- {
			v := h.Events[i]
			ev := v.Event
			if page.Before != 0 && v.Sequence >= page.Before {
				continue
			}

			t := common.Value(ev.EventTime)
			if t < float64(time.Now().Add(-90*24*time.Hour).Unix()) ||
				(in.StartTime != nil && t < *in.StartTime) || (in.EndTime != nil && t > *in.EndTime) {
				continue
			}

			if len(in.LookupAttributes) > 0 && !matches(&ev, in.LookupAttributes[0]) {
				continue
			}

			if len(out.Events) == limit {
				b, e := json.Marshal(cursor{Before: last, Filter: fingerprint})
				if e != nil {
					return e
				}

				out.NextToken = new(base64.RawURLEncoding.EncodeToString(b))
				break
			}

			out.Events = append(out.Events, ev)
			last = v.Sequence
		}

		return nil
	})
	return out, err
}
