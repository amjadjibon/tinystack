// Package mq stores broker metadata and optionally runs local Docker messaging engines.
package mq

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "mq"
	brokerKind  = "broker"
	replayKind  = "replay"
	activeMQ    = "ACTIVEMQ"
	rabbitMQ    = "RABBITMQ"
	single      = "SINGLE_INSTANCE"
	standby     = "ACTIVE_STANDBY_MULTI_AZ"
	cluster     = "CLUSTER_MULTI_AZ"
	micro       = "mq.t3.micro"
	large       = "mq.m5.large"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 metadata inside the broker's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}
type Service struct {
	store   storage.Store
	network Network
	runtime *brokerRuntime
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

// ProtectCredentials applies local credential redaction: the pinned MQ model does not
// mark Password members sensitive. Creation and user mutations contain credentials.
//
//nolint:gocritic // Startup decoration preserves the generated value descriptor contract.
func ProtectCredentials(d catalog.Service) catalog.Service {
	op := d.Operations["CreateBroker"]
	op.Sensitive = true

	d.Operations["CreateBroker"] = op
	for _, name := range []string{"CreateUser", "UpdateUser"} {
		u := d.Operations[name]
		u.Sensitive = true
		d.Operations[name] = u
	}
	return d
}

func invalid(message string) error { return awserror.New("BadRequestException", message) }
func missing() error {
	e := awserror.New("NotFoundException", "Resource does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func conflict(message string) error {
	e := awserror.New("ConflictException", message)
	e.StatusCode = 409
	return e
}

type record struct {
	UserDetails []api.DescribeUserResponse
	Broker      *api.DescribeBrokerResponse
	Credentials []api.User `json:",omitempty"`
	// InitialUsernames also retains RabbitMQ's initial user metadata, which its
	// DescribeBroker UserSummary contract does not expose.
	InitialUsernames []string
	Restarted        bool `json:",omitempty"`
}
type replay struct {
	ID, ARN, Digest string
	Salt            []byte
}

func read(tx storage.ReadTx, scope request.Scope, id string) (*record, error) {
	if !idPattern.MatchString(id) {
		return nil, invalid("BrokerId must be an Amazon MQ broker ID, not a name or ARN")
	}
	r, ok, e := storage.Read[record](tx, scope.Key(serviceName, brokerKind, id))
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	if len(r.Credentials) > 0 {
		key := scope.Key(serviceName, brokerKind, *r.Broker.BrokerId)
		if err := storage.Write(tx, "aws/mq/index/"+key, key); err != nil {
			return err
		}
	}

	return storage.Write(tx, scope.Key(serviceName, brokerKind, *r.Broker.BrokerId), r)
}

func fingerprint(in *api.CreateBrokerRequest, salt []byte) (string, error) {
	raw, e := json.Marshal(in)
	if e != nil {
		return "", e
	}
	h := hmac.New(sha256.New, salt)
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) CreateBroker(ctx context.Context, in *api.CreateBrokerRequest) (*api.CreateBrokerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	subnets, groups := slices.Clone(in.SubnetIds), slices.Clone(in.SecurityGroups)
	in = common.Clone(in)
	in.SubnetIds, in.SecurityGroups = subnets, groups
	if validationErr := validateCreate(in, s.runtime != nil); validationErr != nil {
		return nil, validationErr
	}
	b := &api.DescribeBrokerResponse{
		BrokerName:                 in.BrokerName,
		DeploymentMode:             in.DeploymentMode,
		EngineType:                 in.EngineType,
		EngineVersion:              in.EngineVersion,
		HostInstanceType:           in.HostInstanceType,
		PubliclyAccessible:         in.PubliclyAccessible,
		AutoMinorVersionUpgrade:    in.AutoMinorVersionUpgrade,
		EncryptionOptions:          in.EncryptionOptions,
		Logs:                       &api.LogsSummary{General: in.Logs.General, Audit: in.Logs.Audit},
		MaintenanceWindowStartTime: in.MaintenanceWindowStartTime,
		SecurityGroups:             in.SecurityGroups,
		SubnetIds:                  in.SubnetIds,
		Tags:                       in.Tags,
	}
	r := &record{Broker: b, InitialUsernames: []string{}}
	for _, u := range in.Users {
		r.InitialUsernames = append(r.InitialUsernames, *u.Username)
		if *in.EngineType == activeMQ {
			b.Users = append(b.Users, api.UserSummary{Username: u.Username})
			r.UserDetails = append(
				r.UserDetails,
				api.DescribeUserResponse{
					Username:      u.Username,
					ConsoleAccess: new(common.Value(u.ConsoleAccess)),
					Groups:        u.Groups,
				},
			)
		}
	}

	if s.runtime != nil {
		r.Credentials = *common.Clone(new(in.Users))
		b.BrokerState = new(brokerCreating)
	}

	id := "b-" + uuid.NewV4().String()
	b.BrokerId = new(id)
	b.BrokerArn = new(fmt.Sprintf("arn:aws:mq:%s:%s:broker:%s:%s", scope.Region, scope.AccountID, *b.BrokerName, id))
	b.Created = new(time.Now().UTC())
	for i := range r.UserDetails {
		r.UserDetails[i].BrokerId = b.BrokerId
	}
	salt := make([]byte, 32)
	if _, e = rand.Read(salt); e != nil {
		return nil, e
	}
	var out *api.CreateBrokerResponse
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.Configuration != nil {
			if err := s.selectConfiguration(tx, scope, b, in.Configuration, false); err != nil {
				return err
			}
		}
		// Idempotency is tested before live network references: replay does not create
		// a second broker and remains valid if an EC2 reference was later removed.
		if in.CreatorRequestId != nil {
			key := scope.Key(serviceName, replayKind, *in.CreatorRequestId)
			prior, ok, err := storage.Read[replay](tx, key)
			if err != nil {
				return err
			}
			if ok {
				digest, err := fingerprint(in, prior.Salt)
				if err != nil {
					return err
				}
				if digest != prior.Digest {
					return conflict("CreatorRequestId was used with different creation parameters")
				}
				if _, err = read(tx, scope, prior.ID); err != nil {
					return conflict("CreatorRequestId belongs to a deleted broker")
				}
				out = &api.CreateBrokerResponse{BrokerId: new(prior.ID), BrokerArn: new(prior.ARN)}
				return nil
			}
		}
		rows, err := storage.Records[record](tx, scope.Prefix(serviceName, brokerKind))
		if err != nil {
			return err
		}
		for _, other := range rows {
			if *other.Broker.BrokerName == *b.BrokerName {
				return conflict("BrokerName already exists")
			}
		}
		if networkErr := s.validateNetwork(tx, scope, b); networkErr != nil {
			return networkErr
		}
		if s.runtime != nil {
			if err := storage.Write(tx, "aws/mq/runtime-owner", s.runtime.owner); err != nil {
				return err
			}
		}

		if saveErr := save(tx, scope, r); saveErr != nil {
			return saveErr
		}
		if in.CreatorRequestId != nil {
			digest, err := fingerprint(in, salt)
			if err != nil {
				return err
			}
			if writeErr := storage.Write(tx, scope.Key(serviceName, replayKind, *in.CreatorRequestId),
				&replay{ID: id, ARN: *b.BrokerArn, Digest: digest, Salt: salt}); writeErr != nil {
				return writeErr
			}
		}
		out = &api.CreateBrokerResponse{BrokerId: b.BrokerId, BrokerArn: b.BrokerArn}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(out), nil
}

func (s *Service) DescribeBroker(
	ctx context.Context,
	in *api.DescribeBrokerRequest,
) (*api.DescribeBrokerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var out *api.DescribeBrokerResponse
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := read(tx, scope, common.Value(in.BrokerId))
		if err == nil {
			out = common.Clone(r.Broker)
		}
		return err
	})
	return out, e
}

func (s *Service) DeleteBroker(ctx context.Context, in *api.DeleteBrokerRequest) (*api.DeleteBrokerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, common.Value(in.BrokerId))
		if err != nil {
			return err
		}
		if s.runtime != nil {
			r.Broker.BrokerState = new(brokerDeleting)
			r.Broker.BrokerInstances = nil
			return save(tx, scope, r)
		}
		tx.Delete(scope.Key(serviceName, brokerKind, *r.Broker.BrokerId))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteBrokerResponse{BrokerId: common.Clone(in.BrokerId)}, nil
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, brokerKind+"/") {
				continue
			}
			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}
			b := r.Broker
			out = append(out, catalog.Resource{
				Type: brokerKind, Name: *b.BrokerName, ARN: *b.BrokerArn,
				Account: account, Region: region, Tags: maps.Clone(b.Tags), Details: map[string]any{
					"RecordsOnly":      s.runtime == nil,
					"ExecutionReady":   common.Value(b.BrokerState) == brokerRunning,
					"Broker":           common.Clone(b),
					"InitialUsernames": slices.Clone(r.InitialUsernames),
				},
			})
		}
		return nil
	})
	return out, e
}
