// Package transfer persists Transfer Family configuration and public-key records.
// Optional loopback SFTP listeners expose S3-backed files.
package transfer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "transfer"
	serverKind     = "server"
	userKind       = "user"
	serviceManaged = "SERVICE_MANAGED"
	directoryPath  = "PATH"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	buckets    *s3.Service
	authorizer catalog.ServiceAuthorizer
	listeners  map[string]*listener
	mu         sync.Mutex
	listening  bool
}

// Snapshot captures actual live configuration, tags and public-key IDs for CF compensation.
type Snapshot struct {
	Server      *api.DescribedServer
	User        *api.DescribedUser
	ServerID    string
	Revision    string
	Incarnation string
	Address     string
	HostKey     []byte
}

func New(store storage.Store) *Service {
	return &Service{store: store, listeners: map[string]*listener{}}
}

func invalid(message string) error { return awserror.New("InvalidRequestException", message) }
func missing() error {
	e := awserror.New("ResourceNotFoundException", "Resource does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func duplicate() error {
	e := awserror.New("ResourceExistsException", "Resource already exists")
	e.StatusCode = 409
	return e
}

func revision() string                { return uuid.NewV4().String() }
func identifier(prefix string) string { return prefix + strings.ReplaceAll(revision(), "-", "")[:17] }

func serverARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:transfer:%s:%s:server/%s", scope.Region, scope.AccountID, id)
}

func userARN(scope request.Scope, id, name string) string {
	return fmt.Sprintf("arn:aws:transfer:%s:%s:user/%s/%s", scope.Region, scope.AccountID, id, name)
}

func recordKey(scope request.Scope, r *Snapshot) string {
	if r.User != nil {
		return scope.Key(serviceName, userKind, r.ServerID+"/"+*r.User.UserName)
	}

	return scope.Key(serviceName, serverKind, r.ServerID)
}

func save(tx storage.WriteTx, scope request.Scope, r *Snapshot) error {
	return storage.Write(tx, recordKey(scope, r), r)
}

func readServer(tx storage.ReadTx, scope request.Scope, id string) (*Snapshot, error) {
	if !serverPattern.MatchString(id) {
		return nil, invalid("ServerId must be s- followed by 17 lowercase hexadecimal digits")
	}

	r, ok, e := storage.Read[Snapshot](tx, scope.Key(serviceName, serverKind, id))
	if e != nil {
		return nil, e
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func readUser(tx storage.ReadTx, scope request.Scope, id, name string) (*Snapshot, error) {
	if !userPattern.MatchString(name) {
		return nil, invalid(
			"UserName requires 3-100 ASCII letters, digits, _, -, . or @; start with a letter, digit or _",
		)
	}

	parent, e := readServer(tx, scope, id)
	if e != nil {
		return nil, e
	}

	if *parent.Server.IdentityProviderType != serviceManaged {
		return nil, invalid("Users require SERVICE_MANAGED identity")
	}

	r, ok, e := storage.Read[Snapshot](tx, scope.Key(serviceName, userKind, id+"/"+name))
	if e != nil {
		return nil, e
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func userCount(tx storage.ReadTx, scope request.Scope, id string) int64 {
	return int64(len(tx.Keys(scope.Prefix(serviceName, userKind) + id + "/")))
}

func (s *Service) CreateServer(ctx context.Context, in *api.CreateServerRequest) (*api.CreateServerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("CreateServer input is required")
	}

	if in.Tags != nil && len(in.Tags) == 0 {
		return nil, invalid("Explicit Tags requires 1-50 pairs")
	}

	in = common.Clone(in)
	if in.Domain == nil {
		in.Domain = new("S3")
	}

	if in.IdentityProviderType == nil {
		in.IdentityProviderType = new(serviceManaged)
	}

	r := &Snapshot{ServerID: identifier("s-"), Revision: revision(), Incarnation: revision()}
	r.Server = &api.DescribedServer{
		ServerId:                     new(r.ServerID),
		Arn:                          new(serverARN(scope, r.ServerID)),
		Domain:                       in.Domain,
		EndpointType:                 in.EndpointType,
		IdentityProviderType:         in.IdentityProviderType,
		Protocols:                    in.Protocols,
		SecurityPolicyName:           in.SecurityPolicyName,
		PreAuthenticationLoginBanner: in.PreAuthenticationLoginBanner,
		Tags:                         in.Tags,
	}

	if configErr := ValidateServerConfiguration(r.Server); configErr != nil {
		return nil, configErr
	}

	if s.listening {
		if callErr := makeHostKey(r); callErr != nil {
			return nil, callErr
		}

		r.Server.State = new("STARTING")
	}

	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(recordKey(scope, r)); ok {
			return duplicate()
		}

		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}

	if s.listening {
		if callErr := s.start(ctx, scope, r.ServerID); callErr != nil {
			return nil, callErr
		}
	}

	return &api.CreateServerResponse{ServerId: new(r.ServerID)}, nil
}

func (s *Service) DescribeServer(
	ctx context.Context,
	in *api.DescribeServerRequest,
) (*api.DescribeServerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("DescribeServer input is required")
	}

	var out *api.DescribedServer
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := readServer(tx, scope, common.Value(in.ServerId))
		if err != nil {
			return err
		}

		out = common.Clone(r.Server)
		out.UserCount = new(userCount(tx, scope, r.ServerID))
		return nil
	})
	if e != nil {
		return nil, e
	}

	return &api.DescribeServerResponse{Server: out}, nil
}

func (s *Service) DeleteServer(ctx context.Context, in *api.DeleteServerRequest) (*api.DeleteServerOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("DeleteServer input is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readServer(tx, scope, common.Value(in.ServerId))
		if err != nil {
			return err
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, userKind) + r.ServerID + "/") {
			tx.Delete(k)
		}

		tx.Delete(recordKey(scope, r))
		return nil
	})
	if e != nil {
		return nil, e
	}

	_ = s.closeListener(scope.Key(serviceName, serverKind, common.Value(in.ServerId)))
	return &api.DeleteServerOutput{}, nil
}

func (s *Service) CreateUser(ctx context.Context, in *api.CreateUserRequest) (*api.CreateUserResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("CreateUser input is required")
	}

	if in.Tags != nil && len(in.Tags) == 0 {
		return nil, invalid("Explicit Tags requires 1-50 pairs")
	}

	in = common.Clone(in)
	r := &Snapshot{ServerID: common.Value(in.ServerId), Revision: revision(), Incarnation: revision()}
	if in.HomeDirectoryType == nil {
		in.HomeDirectoryType = new(directoryPath)
	}

	r.User = &api.DescribedUser{
		Arn:                   new(userARN(scope, r.ServerID, common.Value(in.UserName))),
		UserName:              in.UserName,
		HomeDirectoryMappings: in.HomeDirectoryMappings,
		Role:                  in.Role,
		HomeDirectory:         in.HomeDirectory,
		HomeDirectoryType:     in.HomeDirectoryType,
		Tags:                  in.Tags,
	}

	if configErr := ValidateUserConfiguration(r.User, scope); configErr != nil {
		return nil, configErr
	}

	if in.SshPublicKeyBody != nil {
		key, err := publicKey(*in.SshPublicKeyBody)
		if err != nil {
			return nil, err
		}

		r.User.SshPublicKeys = []api.SshPublicKey{newPublicKey(key)}
	}

	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		parent, err := readServer(tx, scope, r.ServerID)
		if err != nil {
			return err
		}

		if *parent.Server.IdentityProviderType != serviceManaged {
			return invalid("Users require SERVICE_MANAGED identity")
		}

		if _, ok := tx.Get(recordKey(scope, r)); ok {
			return duplicate()
		}

		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}

	return &api.CreateUserResponse{ServerId: new(r.ServerID), UserName: common.Clone(r.User.UserName)}, nil
}

func (s *Service) DescribeUser(ctx context.Context, in *api.DescribeUserRequest) (*api.DescribeUserResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("DescribeUser input is required")
	}

	var r *Snapshot
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		r, err = readUser(tx, scope, common.Value(in.ServerId), common.Value(in.UserName))
		return err
	})
	if e != nil {
		return nil, e
	}

	return &api.DescribeUserResponse{ServerId: new(r.ServerID), User: common.Clone(r.User)}, nil
}

func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if in == nil {
		return nil, invalid("DeleteUser input is required")
	}

	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readUser(tx, scope, common.Value(in.ServerId), common.Value(in.UserName))
		if err != nil {
			return err
		}

		tx.Delete(recordKey(scope, r))
		return nil
	})
	if e != nil {
		return nil, e
	}

	return &api.DeleteUserOutput{}, nil
}

func tagsOf(r *Snapshot) []api.Tag {
	if r.User != nil {
		return r.User.Tags
	}

	return r.Server.Tags
}

func setTags(r *Snapshot, tags []api.Tag) {
	if r.User != nil {
		r.User.Tags = tags
	} else {
		r.Server.Tags = tags
	}
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || (!strings.HasPrefix(rest, serverKind+"/") && !strings.HasPrefix(rest, userKind+"/")) {
				continue
			}

			r, _, err := storage.Read[Snapshot](tx, k)
			if err != nil {
				return err
			}

			kind, name, arn := serverKind, r.ServerID, ""
			var metadata any
			if r.User != nil {
				kind = userKind
				name = r.ServerID + "/" + *r.User.UserName
				arn = *r.User.Arn
				metadata = common.Clone(r.User)
			} else {
				arn = *r.Server.Arn
				metadata = common.Clone(r.Server)
			}

			out = append(
				out,
				catalog.Resource{
					Type:    kind,
					Name:    name,
					ARN:     arn,
					Account: account,
					Region:  region,
					Tags:    tagMap(tagsOf(r)),
					Details: map[string]any{
						"RecordsOnly":    !s.listening,
						"ExecutionReady": s.listening && r.Server != nil && common.Value(r.Server.State) == "ONLINE",
						"LocalAddress":   r.Address,
						"Configuration":  metadata,
					},
				},
			)
		}

		return nil
	})
	return out, e
}

// ProtectPrivateInputs hides rejected HostKey/credential/private-key bodies in diagnostics.
//
//nolint:gocritic // Startup decoration preserves the generated value descriptor contract.
func ProtectPrivateInputs(d catalog.Service) catalog.Service {
	for _, name := range []string{"CreateServer", "UpdateServer", "CreateUser", "UpdateUser", "ImportSshPublicKey"} {
		op := d.Operations[name]
		op.Sensitive = true
		d.Operations[name] = op
	}

	return d
}
