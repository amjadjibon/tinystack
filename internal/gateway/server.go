package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/awsjson"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

type Server struct {
	services map[string]catalog.Service
	logger   *slog.Logger
	cfg      config.Config
}

//nolint:gocritic // Snapshot scalar server configuration instead of sharing mutable caller fields.
func New(cfg config.Config, logger *slog.Logger, services ...catalog.Service) (*Server, error) {
	if guardErr := cfg.Validate(); guardErr != nil {
		return nil, guardErr
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, logger: logger, services: map[string]catalog.Service{}}
	for _, service := range services {
		if _, exists := s.services[service.Name]; exists {
			return nil, fmt.Errorf("duplicate service %s", service.Name)
		}
		s.services[service.Name] = service
	}
	return s, nil
}

func (s *Server) metadata(r *http.Request) (request.Metadata, error) {
	m := request.Metadata{
		Scope:    request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
		Protocol: catalog.AWSJSON10,
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return m, err
	}
	m.RequestID = hex.EncodeToString(random[:])
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(host, ".")
	if len(parts) == 4 && parts[2] == "amazonaws" && parts[3] == "com" {
		m.Service = parts[0]
		m.Region = parts[1]
	}
	signedService := ""
	if auth := r.Header.Get("Authorization"); auth != "" {
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
			return m, awserror.New("IncompleteSignatureException", "Expected AWS4-HMAC-SHA256")
		}
		credential := ""
		for _, field := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && k == "Credential" {
				credential = v
			}
		}
		scope := strings.Split(credential, "/")
		if len(scope) != 5 || scope[0] == "" || len(scope[1]) != 8 || scope[2] == "" || scope[3] == "" ||
			scope[4] != "aws4_request" {
			return m, awserror.New("IncompleteSignatureException", "Malformed credential scope")
		}
		m.AccessKey = scope[0]
		m.Region = scope[2]
		signedService = scope[3]
		m.Service = signedService
		if account, ok := s.cfg.Identity.Accounts[m.AccessKey]; ok {
			m.AccountID = account
		}
	}
	target, op, ok := strings.Cut(r.Header.Get("X-Amz-Target"), ".")
	if !ok || op == "" {
		return m, awserror.New("UnknownOperationException", "X-Amz-Target must identify an operation")
	}
	found := false
	for _, service := range s.services {
		if service.TargetPrefix == target {
			if signedService != "" && signedService != service.SigningName {
				return m, awserror.Validation("Signing service conflicts with X-Amz-Target")
			}
			m.Service = service.Name
			found = true
			break
		}
	}
	if !found {
		return m, awserror.New("UnknownOperationException", "Unknown target prefix")
	}
	m.Operation = op
	return m, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_tinystack/health" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","services":["dynamodb"],"storage":"memory"}`))
		return
	}
	start := time.Now()
	m, err := s.metadata(r)
	w.Header().Set("x-amzn-RequestId", m.RequestID)
	defer func() {
		s.logger.Info(
			"request",
			"request_id",
			m.RequestID,
			"service",
			m.Service,
			"operation",
			m.Operation,
			"account",
			m.AccountID,
			"region",
			m.Region,
			"duration_ms",
			float64(time.Since(start).Microseconds())/1000,
		)
	}()
	if err != nil {
		awsjson.Write(w, 0, nil, err)
		return
	}
	service := s.services[m.Service]
	op, ok := service.Operations[m.Operation]
	if !ok {
		awsjson.Write(w, 0, nil, awserror.New("UnknownOperationException", "Operation is not implemented"))
		return
	}
	contentType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	binding, supported := op.Bindings[catalog.AWSJSON10]
	if parseErr != nil || !supported || contentType != binding.ContentType {
		awsjson.Write(w, 0, nil, awserror.Validation("Unsupported content type for this operation"))
		return
	}
	if r.Method != binding.Method || r.URL.Path != binding.URI || r.URL.RawQuery != "" {
		awsjson.Write(w, 0, nil, awserror.Validation("Unexpected method, path, or query parameters"))
		return
	}
	input := op.NewInput()
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if guardErr := awsjson.Decode(r.Body, input); guardErr != nil {
		awsjson.Write(w, 0, nil, guardErr)
		return
	}
	output, err := op.Invoke(request.WithMetadata(r.Context(), m), input)
	awsjson.Write(w, binding.StatusCode, output, err)
}
