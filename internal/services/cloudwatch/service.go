// Package cloudwatch implements CloudWatch metrics, alarms, and dashboards
// without HTTP dependencies. Resources belong to an account and region:
//
//	<scope>/cloudwatch/metric/<metric ID>                     (namespace, name, and dimensions)
//	<scope>/cloudwatch/data/<metric ID>/<zero-padded ms><sequence>
//	<scope>/cloudwatch/alarm/<name>
//	<scope>/cloudwatch/history/<alarm>/<zero-padded ms><sequence>
//	<scope>/cloudwatch/dashboard/<name>
//
// A metric's ID hashes its namespace, name, and dimensions. Data points
// older than 15 days are deleted as new ones arrive. The evaluator (started
// by New, stopped by Stop) evaluates every alarm each second and runs the
// actions of a state change (SNS topics, Lambda functions, and Auto Scaling
// policies) in-process through a catalog.Caller; every change also goes to
// EventBridge's default bus as a "CloudWatch Alarm State Change" event.
package cloudwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service       = "cloudwatch"
	kindMetric    = "metric"
	kindData      = "data"
	kindAlarm     = "alarm"
	kindHistory   = "history"
	kindDashboard = "dashboard"
	retention     = 15 * 24 * time.Hour
	stateOK       = "OK"
	stateAlarm    = "ALARM"
	stateNoData   = "INSUFFICIENT_DATA"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	logger *slog.Logger
	now    func() time.Time
	bg     *background.Group
	// seq orders records written in the same millisecond.
	seq uint64
	mu  sync.Mutex
}

// New returns a CloudWatch service whose evaluator runs until Stop.
func New(store storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{store: store, logger: logger, now: time.Now, bg: background.New()}
	s.bg.Every(evaluateInterval, s.visit)
	s.bg.Go(s.resumeBilling)
	s.bg.Every(time.Second, s.visitBilling)
	return s
}

// UseCaller lets alarms run their actions. Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// Stop ends the evaluator and waits for it.
func (s *Service) Stop() { s.bg.Stop() }

// sequenced is a key suffix ordering records by time, then arrival.
func (s *Service) sequenced(t time.Time) string {
	s.mu.Lock()
	s.seq++
	n := s.seq
	s.mu.Unlock()
	return fmt.Sprintf("%015d%010d", t.UnixMilli(), n%1e10)
}

func fromUnix(f float64) time.Time { return time.UnixMilli(int64(f * 1000)).UTC() }

func invalidValue(format string, args ...any) error {
	return awserror.New("InvalidParameterValueException", fmt.Sprintf(format, args...))
}

func missing(name string) error {
	return awserror.New("MissingRequiredParameterException", "The parameter "+name+" is required.")
}

// metric is a metric's record.
type metric struct {
	Namespace  string
	Name       string
	Dimensions []api.Dimension
	// Last is when a data point last arrived.
	Last float64
}

// canonical sorts dimensions by name.
func canonical(dims []api.Dimension) []api.Dimension {
	out := slices.Clone(dims)
	slices.SortFunc(
		out,
		func(a, b api.Dimension) int { return strings.Compare(common.Value(a.Name), common.Value(b.Name)) },
	)
	return out
}

// metricID identifies a metric by its namespace, name, and dimensions.
func metricID(namespace, name string, dims []api.Dimension) string {
	h := sha256.New()
	h.Write([]byte(namespace + "\x00" + name))
	for _, d := range canonical(dims) {
		h.Write([]byte("\x00" + common.Value(d.Name) + "=" + common.Value(d.Value)))
	}

	return hex.EncodeToString(h.Sum(nil))[:32]
}

func checkDimensions(dims []api.Dimension) error {
	if len(dims) > 30 {
		return invalidValue("A metric can have at most 30 dimensions")
	}

	seen := map[string]bool{}
	for _, d := range dims {
		name, v := common.Value(d.Name), common.Value(d.Value)
		switch {
		case name == "" || len(name) > 255 || v == "" || len(v) > 1024:
			return invalidValue("Dimension names must be 1 to 255 characters and values 1 to 1024")
		case seen[name]:
			return awserror.New("InvalidParameterCombinationException", "Duplicate dimension "+name)
		}

		seen[name] = true
	}

	return nil
}

func alarmARN(scope request.Scope, name string) string {
	return "arn:aws:cloudwatch:" + scope.Region + ":" + scope.AccountID + ":alarm:" + name
}

func dashboardARN(scope request.Scope, name string) string {
	return "arn:aws:cloudwatch::" + scope.AccountID + ":dashboard/" + name
}

func tagsOf(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[common.Value(t.Key)] = common.Value(t.Value)
	}

	return out
}
