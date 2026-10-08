package cloudformation

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/listing"
)

const (
	bucketType = "AWS::S3::Bucket"
	queueType  = "AWS::SQS::Queue"
)

// ResourceProvider exposes the bucket and queue handlers to Cloud Control.
// Reads come from the owning service, including resources created outside it.
// CloudFormation continues to use the same create, update and remove handlers.
type ResourceProvider struct{ caller *catalog.Caller }

func NewResourceProvider(c *catalog.Caller) *ResourceProvider {
	return &ResourceProvider{caller: c.Lenient()}
}

// CheckType restricts the adapter to types with live read and list support.
func (p *ResourceProvider) CheckType(name string) error {
	if name != bucketType && name != queueType {
		return awserror.New("TypeNotFoundException", "TinyStack supports AWS::S3::Bucket and AWS::SQS::Queue")
	}

	return nil
}

func providerInvalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

// Validate rejects unsupported properties and malformed values before writes.
func (p *ResourceProvider) Validate(name string, model map[string]any) error {
	if err := p.CheckType(name); err != nil {
		return err
	}

	for k, v := range model {
		if !slices.Contains(types[name].properties, k) {
			return providerInvalid("TinyStack does not support the %s property of %s", k, name)
		}

		switch k {
		case memberTags:
			tags, ok := v.([]any)
			if !ok {
				return providerInvalid("Tags must be an array of Key and Value objects")
			}

			seen := map[string]bool{}
			for _, tag := range tags {
				m, ok := tag.(map[string]any)
				key, keyOK := m[memberKey].(string)
				_, valueOK := m[memberValue].(string)
				if !ok || len(m) != 2 || !keyOK || !valueOK || key == "" || seen[key] {
					return providerInvalid("Tags require unique nonempty string Keys and string Values")
				}

				seen[key] = true
			}
		case propertyVersioning:
			m, ok := v.(map[string]any)
			if !ok || len(m) != 1 || m[memberStatus] != "Enabled" && m[memberStatus] != "Suspended" {
				return providerInvalid("VersioningConfiguration supports only Status Enabled or Suspended")
			}
		case propertyFifoQueue, memberContentBasedDeduplication, propertyManagedSse:
			if _, ok := v.(bool); !ok {
				return providerInvalid("%s must be a boolean", k)
			}
		case propertyDelaySeconds, propertyMaximumMessageSize, propertyMessageRetention, propertyReceiveWait,
			propertyVisibilityTimeout, propertyKmsReusePeriod:
			if n, ok := v.(float64); !ok || n != float64(int64(n)) {
				return providerInvalid("%s must be an integer", k)
			}
		case propertyRedrivePolicy, propertyRedriveAllowPolicy:
			if _, ok := v.(map[string]any); !ok {
				return providerInvalid("%s must be an object", k)
			}
		default:
			if s, ok := v.(string); !ok || s == "" {
				return providerInvalid("%s must be a nonempty string", k)
			}

			if k == memberBucketName && v != strings.ToLower(v.(string)) {
				return providerInvalid("BucketName must be lowercase")
			}
		}
	}

	return nil
}

func (p *ResourceProvider) client(ctx context.Context) (*client, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return &client{
		caller:   p.caller,
		scope:    m.Scope,
		endpoint: m.Endpoint,
		stack:    "cloudcontrol",
		logical:  "resource",
	}, nil
}

// Name returns the resource name used to serialize Cloud Control writes. Queue
// URL hosts are ignored just as they are by SQS, so aliases share a lock.
func (p *ResourceProvider) Name(ctx context.Context, name, identifier string, model map[string]any) (string, error) {
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}

	if identifier != "" {
		if name == bucketType {
			return identifier, nil
		}

		u, parseErr := url.Parse(identifier)
		if parseErr != nil {
			return "", providerInvalid("Identifier must be a queue URL")
		}

		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != c.scope.AccountID || parts[1] == "" {
			return "", awserror.New("ResourceNotFoundException", "Queue not found in the caller's scope")
		}

		return parts[1], nil
	}

	property := types[name].name
	if n, ok := model[property].(string); ok {
		return n, nil
	}

	n := strings.ToLower(c.generate(63))
	if name == queueType && props(model).truth(propertyFifoQueue) {
		n += ".fifo"
	}

	model[property] = n
	return n, nil
}

// Identifiers lists live resources of one type in the caller's scope.
func (p *ResourceProvider) Identifiers(ctx context.Context, name string) ([]string, error) {
	resources, err := listing.Scoped(ctx, p.caller)
	if err != nil {
		return nil, err
	}

	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, r := range resources {
		if name == bucketType && r.Service == "s3" && r.Type == "bucket" {
			ids = append(ids, r.Name)
		}

		if name == queueType && r.Service == "sqs" && r.Type == "queue" {
			ids = append(ids, c.endpoint+"/"+c.scope.AccountID+"/"+r.Name)
		}
	}

	slices.Sort(ids)
	return ids, nil
}

// Read returns a CloudFormation resource model from the owning service.
func (p *ResourceProvider) Read(ctx context.Context, name, identifier string) (map[string]any, error) {
	n, err := p.Name(ctx, name, identifier, nil)
	if err != nil {
		return nil, err
	}

	ids, err := p.Identifiers(ctx, name)
	if err != nil {
		return nil, err
	}

	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}

	match := n
	if name == queueType {
		match = c.endpoint + "/" + c.scope.AccountID + "/" + n
	}

	if !slices.Contains(ids, match) {
		return nil, awserror.New("ResourceNotFoundException", "Resource not found in the caller's scope")
	}

	if name == bucketType {
		return readBucket(ctx, c, n)
	}

	return readQueue(ctx, c, n, identifier)
}

func readBucket(ctx context.Context, c *client, name string) (map[string]any, error) {
	model := map[string]any{memberBucketName: name, memberArn: "arn:aws:s3:::" + name}
	out, err := c.call(ctx, "s3", "GetBucketVersioning", map[string]any{memberBucket: name})
	if err != nil {
		return nil, err
	}

	if status := outText(out, memberStatus); status != "" {
		model[propertyVersioning] = map[string]any{memberStatus: status}
	}

	out, err = c.call(ctx, "s3", "GetBucketTagging", map[string]any{memberBucket: name})
	if err != nil && !gone(err, "NoSuchTagSet") {
		return nil, err
	}

	if tags, ok := out["TagSet"].([]any); ok && len(tags) > 0 {
		model[memberTags] = tags
	}

	return model, nil
}

func readQueue(ctx context.Context, c *client, name, identifier string) (map[string]any, error) {
	out, err := c.call(ctx, "sqs", "GetQueueAttributes", map[string]any{
		memberQueueURL: identifier, "AttributeNames": []any{"All"},
	})
	if err != nil {
		return nil, err
	}

	attributes, _ := out[memberAttributes].(map[string]any)
	model := map[string]any{memberQueueName: name, memberQueueURL: identifier, memberArn: attributes["QueueArn"]}
	for k, v := range attributes {
		if !slices.Contains(types[queueType].properties, k) {
			continue
		}

		text, _ := v.(string)
		switch k {
		case propertyFifoQueue, memberContentBasedDeduplication, propertyManagedSse:
			model[k], _ = strconv.ParseBool(text)
		case propertyDelaySeconds, propertyMaximumMessageSize, propertyMessageRetention, propertyReceiveWait,
			propertyVisibilityTimeout, propertyKmsReusePeriod:
			model[k], _ = strconv.ParseFloat(text, 64)
		case propertyRedrivePolicy, propertyRedriveAllowPolicy:
			var doc map[string]any
			if decodeErr := json.Unmarshal([]byte(text), &doc); decodeErr != nil {
				return nil, decodeErr
			}

			model[k] = doc
		default:
			if text != "" {
				model[k] = text
			}
		}
	}

	out, err = c.call(ctx, "sqs", "ListQueueTags", map[string]any{memberQueueURL: identifier})
	if err != nil {
		return nil, err
	}

	if tags, ok := out[memberTags].(map[string]any); ok && len(tags) > 0 {
		model[memberTags] = tagList(props{memberTags: tags}.tags(), memberKey, memberValue)
	}

	return model, nil
}

// Create uses the same handler as a stack, after rejecting duplicate names.
func (p *ResourceProvider) Create(ctx context.Context, name string, model map[string]any) (string, error) {
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}

	n := props(model).str(types[name].name)
	id := n
	if name == queueType {
		id = c.endpoint + "/" + c.scope.AccountID + "/" + n
	}

	if _, readErr := p.Read(ctx, name, id); readErr == nil {
		return "", awserror.New("AlreadyExistsException", "A resource with this name already exists")
	} else if !gone(readErr, "ResourceNotFoundException") {
		return "", readErr
	}

	id, _, err = types[name].create(ctx, c, props(model))
	return id, err
}

// Update rejects replacement and read-only changes instead of replacing a resource.
func (p *ResourceProvider) Update(ctx context.Context, name, identifier string, old, model map[string]any) error {
	before, after := maps.Clone(old), maps.Clone(model)
	for _, k := range []string{memberArn, memberQueueURL} {
		if !reflect.DeepEqual(before[k], after[k]) {
			return awserror.New("NotUpdatableException", k+" is read-only")
		}

		delete(before, k)
		delete(after, k)
	}

	if err := p.Validate(name, after); err != nil {
		return err
	}

	if types[name].replaces(props(before), props(after)) {
		return awserror.New("NotUpdatableException", "The update changes a create-only property")
	}

	c, err := p.client(ctx)
	if err != nil {
		return err
	}

	_, err = types[name].update(ctx, c, &resource{
		Type: name, Physical: identifier, Properties: before, Attributes: old,
	}, props(after))
	return err
}

// Delete requires the resource to exist before invoking the shared handler.
func (p *ResourceProvider) Delete(ctx context.Context, name, identifier string) error {
	if _, err := p.Read(ctx, name, identifier); err != nil {
		return err
	}

	c, err := p.client(ctx)
	if err != nil {
		return err
	}

	return types[name].remove(ctx, c, &resource{Type: name, Physical: identifier})
}
