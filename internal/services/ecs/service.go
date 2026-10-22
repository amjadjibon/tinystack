// Package ecs implements Amazon Elastic Container Service without HTTP
// dependencies. Without a Runtime no container runs: a task settles in the
// state an operation asks for at once, and a service keeps its desired count of
// running tasks as records. With one (UseRuntime), tasks start PENDING and a
// runner starts their containers (runtime.go). Resources belong to an account
// and region:
//
//	<scope>/ecs/cluster/<name>
//	<scope>/ecs/family/<family>                          (the next revision)
//	<scope>/ecs/taskdef/<family>/<zero-padded revision>
//	<scope>/ecs/service/<cluster>/<name>
//	<scope>/ecs/task/<cluster>/<task ID>
//	aws/ecs/index/<account>/<region>/<cluster>/<task ID>  (tasks the runner owns)
//
// Deleted clusters and services stay as INACTIVE records, as AWS reports them,
// and a name can be reused; stopped tasks are listed for an hour.
package ecs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	fieldTags      = "TAGS"
	fieldSettings  = "SETTINGS"
	kindCluster    = "cluster"
	kindService    = "service"
	kindTask       = "task"
	service        = "ecs"
	defaultCluster = "default"
	active         = "ACTIVE"
	inactive       = "INACTIVE"
	missing        = "MISSING"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	runtime   Runtime
	writeLogs LogWriter
	logger    *slog.Logger
	now       func() time.Time
	bg        *background.Group
	// busy holds the index keys the runner is advancing, and cursors the time
	// of the last log line shipped from each container.
	busy     map[string]bool
	cursors  map[string]time.Time
	endpoint string
	mu       sync.Mutex
}

func New(store storage.Store) *Service {
	return &Service{
		bg: background.New(), store: store, now: time.Now, busy: map[string]bool{},
		cursors: map[string]time.Time{},
	}
}

// UseEndpoint sets the base URL task containers use to reach TinyStack for metadata and credentials.
func (s *Service) UseEndpoint(endpoint string) {
	s.endpoint = strings.TrimRight(endpoint, "/")
}

// Stop ends the runner and removes every container the runtime started.
func (s *Service) Stop() {
	if s.bg.Stop() && s.runtime != nil {
		s.runtime.Close()
	}
}

// Reset removes every container the runtime started and forgets how far task
// logs were shipped.
func (s *Service) Reset(context.Context) {
	s.mu.Lock()
	clear(s.cursors)
	s.mu.Unlock()
	common.ResetRuntime(s.runtime)
}

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

var errPageToken = invalidParameter("Invalid token")

func clientError(format string, args ...any) error {
	return awserror.New("ClientException", fmt.Sprintf(format, args...))
}

func clusterNotFound() error {
	return awserror.New("ClusterNotFoundException", "Cluster not found.")
}

func arn(scope request.Scope, resource string) string {
	return "arn:aws:ecs:" + scope.Region + ":" + scope.AccountID + ":" + resource
}

func seconds(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}

	return common.SecondsMilli(t)
}

// limit validates an optional page size of 1 to 100, which defaults to def.
func limit(n *int64, def int) (int, error) {
	if n == nil {
		return def, nil
	}

	if *n < 1 || *n > 100 {
		return 0, invalidParameter("maxResults must be between 1 and 100.")
	}

	return int(*n), nil
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)

func checkName(name *string, member string) error {
	if name == nil || !namePattern.MatchString(*name) {
		return invalidParameter("%s can have up to 255 letters (uppercase and lowercase), numbers, underscores, "+
			"and hyphens.", member)
	}

	return nil
}

// resourceName returns the name a name or an ARN of kind ("cluster/",
// "service/<cluster>/", …) in scope refers to.
func resourceName(scope request.Scope, ref, kind string) (string, bool) {
	if !strings.HasPrefix(ref, "arn:") {
		return ref, namePattern.MatchString(ref)
	}

	name, ok := strings.CutPrefix(ref, arn(scope, kind))
	if !ok {
		// Services also have the old ARN form without the cluster.
		if old, cut := strings.CutPrefix(kind, "service/"); cut && old != "" {
			name, ok = strings.CutPrefix(ref, arn(scope, "service/"))
		}
	}

	return name, ok && namePattern.MatchString(name)
}

func oneOf(v *string, allowed ...string) bool {
	return v == nil || slices.Contains(allowed, *v)
}

// digits returns n random decimal digits.
func digits(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}

	return string(b)
}

// newID returns 32 random hexadecimal digits, the form of a task ID.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
