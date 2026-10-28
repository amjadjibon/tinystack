package transfer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type listener struct {
	net.Listener
	connections map[net.Conn]bool
	wg          sync.WaitGroup
	mu          sync.Mutex
	closed      bool
}

func makeHostKey(r *Snapshot) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}

	block, err := ssh.MarshalPrivateKey(key, "TinyStack Transfer")
	if err != nil {
		return err
	}

	r.HostKey = pem.EncodeToMemory(block)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}

	r.Server.HostKeyFingerprint = new(ssh.FingerprintSHA256(signer.PublicKey()))
	return nil
}

// UseServiceAuthorizer applies the shared IAM evaluator to each S3 operation under the user's role.
func (s *Service) UseServiceAuthorizer(a catalog.ServiceAuthorizer) { s.authorizer = a }

// Listen enables SFTP and resumes ONLINE servers from persistent storage.
func (s *Service) Listen(ctx context.Context, buckets *s3.Service) error {
	s.listening, s.buckets = true, buckets
	type server struct {
		scope request.Scope
		id    string
	}

	var servers []server
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || len(rest) < 7 || rest[:7] != "server/" {
				continue
			}

			r, _, e := storage.Read[Snapshot](tx, k)
			if e != nil {
				return e
			}

			if common.Value(r.Server.State) == "ONLINE" || common.Value(r.Server.State) == "STARTING" {
				servers = append(servers, server{request.Scope{AccountID: account, Region: region}, r.ServerID})
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, r := range servers {
		if err := s.start(ctx, r.scope, r.id); err != nil {
			s.Stop()
			return err
		}
	}

	return nil
}

func (s *Service) closeListener(key string) error {
	l := s.listeners[key]
	if l == nil {
		return nil
	}

	delete(s.listeners, key)
	closeErr := l.Close()
	l.mu.Lock()
	l.closed = true
	for c := range l.connections {
		_ = c.Close()
	}

	l.mu.Unlock()
	l.wg.Wait()
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}

	return closeErr
}

// Stop closes all listeners and sessions without changing persisted ONLINE intent.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.listeners {
		_ = s.closeListener(key)
	}
}

// Reset drops listeners before the app clears records.
func (s *Service) Reset(context.Context) { s.Stop() }

func (s *Service) state(ctx context.Context, scope request.Scope, id, state string) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readServer(tx, scope, id)
		if err != nil {
			return err
		}

		r.Server.State = new(state)
		r.Revision = revision()
		return save(tx, scope, r)
	})
}

func (s *Service) start(ctx context.Context, scope request.Scope, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scope.Key(serviceName, serverKind, id)
	var r *Snapshot
	err := s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = readServer(tx, scope, id); return e })
	if err != nil {
		return err
	}

	if s.listeners[key] != nil {
		return nil
	}

	if callErr := s.state(ctx, scope, id, "STARTING"); callErr != nil {
		return callErr
	}

	err = s.open(ctx, scope, r)
	if err != nil {
		stateErr := s.state(ctx, scope, id, "START_FAILED")
		return awserror.New("InternalServiceError", errors.Join(err, stateErr).Error())
	}

	return nil
}

//nolint:contextcheck // Listener sessions outlive the startup/CreateServer request context.
func (s *Service) open(ctx context.Context, scope request.Scope, r *Snapshot) error {
	signer, err := ssh.ParsePrivateKey(r.HostKey)
	if err != nil {
		return err
	}

	address := r.Address
	host, _, addressErr := net.SplitHostPort(address)
	if addressErr != nil || host != "127.0.0.1" {
		address = "127.0.0.1:0"
	}

	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		ln, err = lc.Listen(ctx, "tcp", "127.0.0.1:0")
	}

	if err != nil {
		return err
	}

	l := &listener{Listener: ln, connections: map[net.Conn]bool{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		live, e := readServer(tx, scope, r.ServerID)
		if e != nil {
			return e
		}

		live.Address, live.Server.State = ln.Addr().String(), new("ONLINE")
		live.Revision = revision()
		return save(tx, scope, live)
	})
	if err != nil {
		_ = ln.Close()
		return err
	}

	s.listeners[scope.Key(serviceName, serverKind, r.ServerID)] = l
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			u, e := s.DescribeUser(
				request.WithMetadata(context.Background(), request.Metadata{Scope: scope}),
				&api.DescribeUserRequest{ServerId: new(r.ServerID), UserName: new(c.User())},
			)
			if e != nil {
				return nil, errors.New("authentication failed")
			}

			for _, k := range u.User.SshPublicKeys {
				pub, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(*k.SshPublicKeyBody))
				if parseErr == nil && bytes.Equal(pub.Marshal(), key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}

			return nil, errors.New("authentication failed")
		},
	}

	config.BannerCallback = func(ssh.ConnMetadata) string {
		current, readErr := s.DescribeServer(
			request.WithMetadata(context.Background(), request.Metadata{Scope: scope}),
			&api.DescribeServerRequest{ServerId: new(r.ServerID)},
		)
		if readErr != nil {
			return ""
		}

		return common.Value(current.Server.PreAuthenticationLoginBanner)
	}

	config.AddHostKey(signer)
	l.wg.Go(func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}

			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				_ = conn.Close()
				return
			}

			l.connections[conn] = true
			l.mu.Unlock()
			l.wg.Go(func() {
				defer func() { _ = conn.Close(); l.mu.Lock(); delete(l.connections, conn); l.mu.Unlock() }()
				s.session(conn, config, scope, r.ServerID)
			})
		}
	})
	return nil
}

func (s *Service) session(conn net.Conn, config *ssh.ServerConfig, scope request.Scope, id string) {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}

	defer func() { _ = sshConn.Close() }()
	_ = conn.SetDeadline(time.Time{})
	go ssh.DiscardRequests(requests)
	ctx, cancel := context.WithCancel(request.WithMetadata(context.Background(), request.Metadata{Scope: scope}))
	defer cancel()
	u, err := s.DescribeUser(ctx, &api.DescribeUserRequest{ServerId: new(id), UserName: new(sshConn.User())})
	if err != nil {
		return
	}

	for ch := range channels {
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "SFTP sessions only")
			continue
		}

		channel, reqs, e := ch.Accept()
		if e != nil {
			return
		}
		// One channel is served at a time; the transport closing ends Serve and cleans open handles.
		for req := range reqs {
			if req.Type != "subsystem" || !bytes.Equal(req.Payload, ssh.Marshal(struct{ Name string }{"sftp"})) {
				_ = req.Reply(false, nil)
				continue
			}

			_ = req.Reply(true, nil)
			h := &filesystem{s: s, ctx: ctx, scope: scope, serverID: id, user: u.User}
			start := "/"
			if common.Value(u.User.HomeDirectoryType) == directoryPath {
				start = *u.User.HomeDirectory
			}

			server := sftp.NewRequestServer(
				channel,
				sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h},
				sftp.WithStartDirectory(start),
			)
			_ = server.Serve()
			_ = server.Close()
			break
		}

		_ = channel.Close()
	}
}

func (s *Service) StartServer(ctx context.Context, in *api.StartServerRequest) (*api.StartServerOutput, error) {
	if !s.listening {
		return nil, invalid("StartServer requires transfer.servers=listen")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("StartServer input is required")
	}

	if callErr := s.start(ctx, scope, common.Value(in.ServerId)); callErr != nil {
		return nil, callErr
	}

	return &api.StartServerOutput{}, nil
}

func (s *Service) StopServer(ctx context.Context, in *api.StopServerRequest) (*api.StopServerOutput, error) {
	if !s.listening {
		return nil, invalid("StopServer requires transfer.servers=listen")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("StopServer input is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if callErr := s.state(ctx, scope, common.Value(in.ServerId), "STOPPING"); callErr != nil {
		return nil, callErr
	}

	if closeErr := s.closeListener(scope.Key(serviceName, serverKind, common.Value(in.ServerId))); closeErr != nil {
		stateErr := s.state(ctx, scope, common.Value(in.ServerId), "STOP_FAILED")
		return nil, awserror.New("InternalServiceError", errors.Join(closeErr, stateErr).Error())
	}

	if callErr := s.state(ctx, scope, common.Value(in.ServerId), "OFFLINE"); callErr != nil {
		return nil, callErr
	}

	return &api.StopServerOutput{}, nil
}
