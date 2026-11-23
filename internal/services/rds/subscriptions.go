package rds

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindSubscription   = "es"
	eventDeliveryIndex = "aws/rds/event-delivery/"
)

func (s *Service) checkSubscriptionTopic(ctx context.Context, scope request.Scope, topic string) error {
	prefix := "arn:aws:sns:" + scope.Region + ":" + scope.AccountID + ":"
	name := strings.TrimPrefix(topic, prefix)
	if name == topic || name == "" || strings.Contains(name, ":") || strings.HasSuffix(name, ".fifo") {
		return awserror.New(
			"SNSInvalidTopicFault",
			"A standard SNS topic in the request account and Region is required",
		)
	}

	if s.caller == nil {
		return awserror.New("SNSNoAuthorizationFault", "SNS is unavailable")
	}

	_, err := s.caller.Call(ctx, "sns", "GetTopicAttributes", map[string]any{"TopicArn": topic})
	if err != nil {
		switch awserror.From(err).Code {
		case "NotFound", "NotFoundException":
			return awserror.New("SNSTopicArnNotFoundFault", "SNS topic does not exist")
		case "InvalidParameter", "InvalidParameterValue":
			return awserror.New("SNSInvalidTopicFault", "Invalid SNS topic ARN")
		default:
			return err
		}
	}

	return nil
}

func checkSubscriptionFilters(sub *api.EventSubscription) error {
	source := common.Value(sub.SourceType)
	if (sub.SourceType != nil && !supportedEventSource(source)) || (source == "" && len(sub.SourceIdsList) > 0) {
		return invalid("SourceIds require a supported SourceType")
	}

	for _, category := range sub.EventCategoriesList {
		if !slices.ContainsFunc(eventCategories, func(item api.EventCategoriesMap) bool {
			return (source == "" || source == common.Value(item.SourceType)) &&
				slices.Contains(item.EventCategories, category)
		}) {
			return awserror.New("SubscriptionCategoryNotFoundFault", "Unsupported event category "+category)
		}
	}

	return nil
}

func checkSubscriptionSource(tx storage.ReadTx, scope request.Scope, source, name string) error {
	for kind, typ := range eventSources {
		if typ != source {
			continue
		}

		if _, exists := tx.Get(scope.Key(serviceName, kind, name)); validName(name, 255) && exists {
			return nil
		}

		break
	}

	return awserror.New("SourceNotFoundFault", "Event source "+name+" was not found")
}

func subscriptionStatus(sub *api.EventSubscription) {
	sub.Status = new("active")
	if !common.Value(sub.Enabled) {
		sub.Status = new("disabled")
	}
}

func (s *Service) CreateEventSubscription(
	ctx context.Context, in *api.CreateEventSubscriptionMessage,
) (*api.CreateEventSubscriptionResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.SubscriptionName))
	if !validName(name, 255) {
		return nil, invalid("Invalid SubscriptionName")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if topicErr := s.checkSubscriptionTopic(ctx, scope, common.Value(in.SnsTopicArn)); topicErr != nil {
		return nil, topicErr
	}

	sub := &api.EventSubscription{
		CustomerAwsId:            new(scope.AccountID),
		CustSubscriptionId:       new(name),
		SnsTopicArn:              in.SnsTopicArn,
		SourceType:               in.SourceType,
		EventCategoriesList:      in.EventCategories,
		Enabled:                  common.OrElse(in.Enabled, new(true)),
		SubscriptionCreationTime: new(time.Now().UTC().Format(time.RFC3339Nano)),
		EventSubscriptionArn:     new(arn(scope, kindSubscription, name)),
	}
	for _, id := range in.SourceIds {
		id = strings.ToLower(id)
		if !slices.Contains(sub.SourceIdsList, id) {
			sub.SourceIdsList = append(sub.SourceIdsList, id)
		}
	}

	if filterErr := checkSubscriptionFilters(sub); filterErr != nil {
		return nil, filterErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(scope.Key(serviceName, kindSubscription, name)); exists {
			return awserror.New("SubscriptionAlreadyExistFault", "Event subscription "+name+" already exists")
		}

		for _, id := range sub.SourceIdsList {
			if sourceErr := checkSubscriptionSource(tx, scope, common.Value(sub.SourceType), id); sourceErr != nil {
				return sourceErr
			}
		}

		subscriptionStatus(sub)
		return writeRecord(tx, scope, &record{Name: name, Kind: kindSubscription, Tags: tags, Subscription: sub})
	})
	return &api.CreateEventSubscriptionResult{EventSubscription: sub}, err
}

func (s *Service) DescribeEventSubscriptions(
	ctx context.Context, in *api.DescribeEventSubscriptionsMessage,
) (*api.EventSubscriptionsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	records, err := s.list(ctx, scope, kindSubscription, common.Value(in.SubscriptionName))
	if err != nil {
		return nil, err
	}

	page, marker, err := pageRecords(records, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.EventSubscriptionsMessage{Marker: marker}
	for _, r := range page {
		out.EventSubscriptionsList = append(out.EventSubscriptionsList, *r.Subscription)
	}

	return out, nil
}

func (s *Service) ModifyEventSubscription(
	ctx context.Context, in *api.ModifyEventSubscriptionMessage,
) (*api.ModifyEventSubscriptionResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.SnsTopicArn != nil {
		if topicErr := s.checkSubscriptionTopic(ctx, scope, *in.SnsTopicArn); topicErr != nil {
			return nil, topicErr
		}
	}

	var sub *api.EventSubscription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindSubscription, common.Value(in.SubscriptionName))
		if readErr != nil {
			return readErr
		}

		sub = r.Subscription
		if in.SourceType != nil && *in.SourceType != common.Value(sub.SourceType) {
			for _, id := range sub.SourceIdsList {
				if sourceErr := checkSubscriptionSource(tx, scope, *in.SourceType, id); sourceErr != nil {
					return sourceErr
				}
			}

			sub.SourceType = in.SourceType
		}

		if in.EventCategories != nil {
			sub.EventCategoriesList = in.EventCategories
		}

		if filterErr := checkSubscriptionFilters(sub); filterErr != nil {
			return filterErr
		}

		sub.SnsTopicArn = common.OrElse(in.SnsTopicArn, sub.SnsTopicArn)
		sub.Enabled = common.OrElse(in.Enabled, sub.Enabled)
		subscriptionStatus(sub)
		return writeRecord(tx, scope, r)
	})
	return &api.ModifyEventSubscriptionResult{EventSubscription: sub}, err
}

func (s *Service) DeleteEventSubscription(
	ctx context.Context, in *api.DeleteEventSubscriptionMessage,
) (*api.DeleteEventSubscriptionResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DeleteEventSubscriptionResult{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindSubscription, common.Value(in.SubscriptionName))
		if readErr != nil {
			return readErr
		}

		tx.Delete(scope.Key(serviceName, kindSubscription, r.Name))
		out.EventSubscription = r.Subscription
		out.EventSubscription.Status = new("deleting")
		return nil
	})
	return out, err
}

func (s *Service) changeSubscriptionSource(
	ctx context.Context, subscription, name string, add bool,
) (*api.EventSubscription, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var sub *api.EventSubscription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindSubscription, subscription)
		if readErr != nil {
			return readErr
		}

		sub = r.Subscription
		name = strings.ToLower(name)
		index := slices.Index(sub.SourceIdsList, name)
		switch {
		case add:
			if sourceErr := checkSubscriptionSource(tx, scope, common.Value(sub.SourceType), name); sourceErr != nil {
				return sourceErr
			}

			if index < 0 {
				sub.SourceIdsList = append(sub.SourceIdsList, name)
			}
		case index < 0:
			return awserror.New("SourceNotFoundFault", "Event source is not in the subscription")
		default:
			sub.SourceIdsList = slices.Delete(sub.SourceIdsList, index, index+1)
		}

		return writeRecord(tx, scope, r)
	})
	return sub, err
}

func (s *Service) AddSourceIdentifierToSubscription(
	ctx context.Context, in *api.AddSourceIdentifierToSubscriptionMessage,
) (*api.AddSourceIdentifierToSubscriptionResult, error) {
	sub, err := s.changeSubscriptionSource(
		ctx,
		common.Value(in.SubscriptionName),
		common.Value(in.SourceIdentifier),
		true,
	)
	return &api.AddSourceIdentifierToSubscriptionResult{EventSubscription: sub}, err
}

func (s *Service) RemoveSourceIdentifierFromSubscription(
	ctx context.Context, in *api.RemoveSourceIdentifierFromSubscriptionMessage,
) (*api.RemoveSourceIdentifierFromSubscriptionResult, error) {
	sub, err := s.changeSubscriptionSource(
		ctx,
		common.Value(in.SubscriptionName),
		common.Value(in.SourceIdentifier),
		false,
	)
	return &api.RemoveSourceIdentifierFromSubscriptionResult{EventSubscription: sub}, err
}

// The outbox records matching subscriptions at event commit time, even if they change before delivery.
type eventDelivery struct {
	Topic, Body string
	Next        time.Time
	Expires     time.Time
	Scope       request.Scope
}

func queueEventDeliveries(
	tx storage.WriteTx, scope request.Scope, event *api.Event, tags map[string]string, id string,
) error {
	var topics []string
	for _, key := range tx.Keys(scope.Prefix(serviceName, kindSubscription)) {
		r, found, err := storage.Read[record](tx, key)
		if err != nil {
			return err
		}

		if !found {
			continue
		}

		sub := r.Subscription
		if !common.Value(sub.Enabled) || !matchesValue(sub.SourceType, *event.SourceType) ||
			(len(sub.SourceIdsList) > 0 && !slices.Contains(sub.SourceIdsList, *event.SourceIdentifier)) ||
			(len(sub.EventCategoriesList) > 0 && !slices.Contains(sub.EventCategoriesList, event.EventCategories[0])) {
			continue
		}

		topic := *sub.SnsTopicArn
		if !slices.Contains(topics, topic) {
			topics = append(topics, topic)
		}
	}

	if len(topics) == 0 {
		return nil
	}

	linkType := map[string]string{
		sourceDBInstance: "dbinstance", sourceDBCluster: "dbclusters",
		sourceDBSnapshot: "dbsnapshot", sourceDBClusterSnapshot: "dbclustersnapshot",
	}[*event.SourceType]
	if tags == nil {
		tags = map[string]string{}
	}

	message := map[string]any{
		"Event Source": *event.SourceType, "Event Time": event.Date.Format("2006-01-02 15:04:05.000"),
		"Identifier Link": "https://console.aws.amazon.com/rds/home?region=" + scope.Region +
			"#" + linkType + ":id=" + *event.SourceIdentifier,
		"Source ID": *event.SourceIdentifier, "Source ARN": *event.SourceArn, "Event Message": *event.Message,
		"Tags": tags,
	}
	if code := rdsEventCode(*event.Message); code != "" {
		message["Event ID"] = "http://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_Events.Messages.html#" + code
	}

	body, err := json.Marshal(message)
	if err != nil {
		return err
	}

	for _, topic := range topics {
		key := eventDeliveryIndex + scope.AccountID + "/" + scope.Region + "/" + id + "/" + topic
		if err := storage.Write(tx, key, &eventDelivery{
			Scope: scope, Topic: topic, Body: string(body), Expires: event.Date.Add(time.Hour),
		}); err != nil {
			return err
		}
	}

	return nil
}

func rdsEventCode(message string) string {
	switch message {
	case "DB instance created", "Created db-instance":
		return "RDS-EVENT-0005"
	case "DB instance deleted":
		return "RDS-EVENT-0003"
	case "DB instance restarted":
		return "RDS-EVENT-0006"
	case "DB instance stop":
		return "RDS-EVENT-0087"
	case "DB instance start":
		return "RDS-EVENT-0088"
	case "DB cluster created", "Created db-cluster":
		return "RDS-EVENT-0170"
	case "DB cluster deleted":
		return "RDS-EVENT-0171"
	case "DB cluster started":
		return "RDS-EVENT-0151"
	case "Automated cluster snapshot created":
		return "RDS-EVENT-0169"
	case "Created db-cluster-snapshot":
		return "RDS-EVENT-0075"
	case "DB cluster stopped":
		return "RDS-EVENT-0150"
	case "DB cluster failover completed":
		return "RDS-EVENT-0071"
	default:
		return ""
	}
}

func (s *Service) visitEventDeliveries(ctx context.Context) {
	if s.caller == nil {
		return
	}

	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(eventDeliveryIndex)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		s.deliverEvent(ctx, key)
	}
}

func (s *Service) deliverEvent(ctx context.Context, key string) {
	var delivery *eventDelivery
	now := time.Now()
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		item, found, err := storage.Read[eventDelivery](tx, key)
		if err != nil || !found {
			return err
		}

		if !now.Before(item.Expires) {
			tx.Delete(key)
			return nil
		}

		if now.Before(item.Next) {
			return nil
		}

		delivery = item
		delivery.Next = now.Add(10 * time.Second)
		return storage.Write(tx, key, delivery)
	})
	if err != nil || delivery == nil {
		return
	}

	deliveryCtx := request.WithMetadata(ctx, request.Metadata{Scope: delivery.Scope, Service: "sns"})
	_, err = s.caller.Call(deliveryCtx, "sns", "Publish", map[string]any{
		"TopicArn": delivery.Topic, "Message": delivery.Body, "Subject": "RDS Notification Message",
	})
	if err == nil {
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(key)
			return nil
		})
	}
}
