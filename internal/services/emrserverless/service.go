// Package emrserverless stores application and job metadata. Nothing executes: application transitions
// finish synchronously, and each GetJobRun observes one simulated state before advancing to the next.
package emrserverless

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	stateCreated   = "CREATED"
	stateStarted   = "STARTED"
	stateStopped   = "STOPPED"
	stateSuccess   = "SUCCESS"
	stateCancelled = "CANCELLED"
	typeHive       = "Hive"
	typeSpark      = "Spark"
	serviceName    = "emrserverless"
	recordsOnly    = "Simulated metadata only; no code executes and no compute resources are provisioned."
)

var (
	_              api.API = (*Service)(nil)
	releasePattern         = regexp.MustCompile(`^emr-\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$`)
	rolePattern            = regexp.MustCompile(
		`^arn:aws(?:-us-gov|-cn)?:iam::\d{12}:role/[A-Za-z0-9_+=,.@/-]+$`,
	)
	cpuPattern             = regexp.MustCompile(`^[1-9]\d*\s?(?:vCPU|vcpu|VCPU)?$`)
	memoryPattern          = regexp.MustCompile(`^[1-9]\d*\s?(?:GB|gb|gB|Gb)?$`)
	diskPattern            = regexp.MustCompile(`^[1-9]\d*\s?(?:GB|gb|gB|Gb)$`)
	applicationNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/#-]{1,64}$`)
	clientTokenPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	applicationStates      = []string{
		"CREATING",
		stateCreated,
		"STARTING",
		stateStarted,
		"STOPPING",
		stateStopped,
		"TERMINATED",
	}

	jobStates = []string{
		"SUBMITTED",
		"PENDING",
		"SCHEDULED",
		"RUNNING",
		stateSuccess,
		"FAILED",
		"CANCELLING",
		stateCancelled,
		"QUEUED",
	}

	progression = []string{"SUBMITTED", "PENDING", "SCHEDULED", "RUNNING", stateSuccess}
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

func missing() error {
	return awserror.New("ResourceNotFoundException", "The application or job run does not exist.")
}

func conflict(message string) error { return awserror.New("ConflictException", message) }

func appKey(scope request.Scope, id string) string { return scope.Key(serviceName, "application", id) }

func jobKey(scope request.Scope, app, id string) string {
	return scope.Key(serviceName, "jobrun", app+"/"+id)
}

func applicationARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:emr-serverless:%s:%s:/applications/%s", scope.Region, scope.AccountID, id)
}

func jobARN(scope request.Scope, app, id string) string {
	return applicationARN(scope, app) + "/jobruns/" + id
}

func readApplication(tx storage.ReadTx, scope request.Scope, id *string) (*api.Application, error) {
	r, found, err := storage.Read[api.Application](tx, appKey(scope, common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func readJob(tx storage.ReadTx, scope request.Scope, app, id *string) (*api.JobRun, error) {
	if _, err := readApplication(tx, scope, app); err != nil {
		return nil, err
	}

	r, found, err := storage.Read[api.JobRun](tx, jobKey(scope, common.Value(app), common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func active(state *string) bool {
	return !slices.Contains([]string{stateSuccess, "FAILED", stateCancelled}, common.Value(state))
}

func hasActiveJobs(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	rows, err := storage.Records[api.JobRun](tx, scope.Prefix(serviceName, "jobrun")+id+"/")
	if err != nil {
		return false, err
	}

	for _, r := range rows {
		if active(r.State) {
			return true, nil
		}
	}

	return false, nil
}

func pageSize(limit *int64) (int, error) {
	if limit == nil {
		return 50, nil
	}

	if *limit < 1 || *limit > 50 {
		return 0, awserror.Validation("maxResults must be between 1 and 50.")
	}

	return int(*limit), nil
}

func checkStates(states, allowed []string) error {
	for _, v := range states {
		if !slices.Contains(allowed, v) {
			return awserror.Validation("Invalid state %q.", v)
		}
	}

	return nil
}

func checkName(name *string) error {
	if name != nil && (strings.TrimSpace(*name) == "" || len(*name) > 256) {
		return awserror.Validation("name must contain 1 to 256 characters.")
	}

	return nil
}

func checkResources(cpu, memory, disk *string) error {
	if !cpuPattern.MatchString(common.Value(cpu)) || len(common.Value(cpu)) > 15 ||
		!memoryPattern.MatchString(common.Value(memory)) || len(common.Value(memory)) > 15 {
		return awserror.Validation("cpu and memory must be positive integer quantities in vCPU and GB.")
	}

	if disk != nil && (!diskPattern.MatchString(*disk) || len(*disk) > 15) {
		return awserror.Validation("disk must be a positive integer quantity in GB.")
	}

	return nil
}

func checkApplication(a *api.Application) error {
	if a.AutoStartConfiguration != nil && a.AutoStartConfiguration.Enabled == nil {
		a.AutoStartConfiguration.Enabled = new(true)
	}

	if a.AutoStopConfiguration != nil {
		if a.AutoStopConfiguration.Enabled == nil {
			a.AutoStopConfiguration.Enabled = new(true)
		}

		if a.AutoStopConfiguration.IdleTimeoutMinutes == nil {
			a.AutoStopConfiguration.IdleTimeoutMinutes = new(int64(15))
		}
	}

	if !releasePattern.MatchString(common.Value(a.ReleaseLabel)) || len(common.Value(a.ReleaseLabel)) > 64 {
		return awserror.Validation("releaseLabel must be an EMR release label, such as emr-7.0.0.")
	}

	if !slices.Contains([]string{typeSpark, typeHive}, common.Value(a.Type)) {
		return awserror.Validation("type must be Spark or Hive.")
	}

	if a.Name != nil && !applicationNamePattern.MatchString(*a.Name) {
		return awserror.Validation("Application name must be 1 to 64 letters, digits or ._/#- characters.")
	}

	if !slices.Contains([]string{"X86_64", "ARM64"}, common.Value(a.Architecture)) {
		return awserror.Validation("architecture must be X86_64 or ARM64.")
	}

	if a.AutoStopConfiguration != nil && a.AutoStopConfiguration.IdleTimeoutMinutes != nil &&
		(*a.AutoStopConfiguration.IdleTimeoutMinutes < 1 || *a.AutoStopConfiguration.IdleTimeoutMinutes > 10080) {
		return awserror.Validation("idleTimeoutMinutes must be between 1 and 10080.")
	}

	for worker, c := range a.InitialCapacity {
		allowed := []string{"Driver", "Executor"}
		if common.Value(a.Type) == typeHive {
			allowed = []string{"HiveDriver", "TezTask"}
		}

		if !slices.Contains(allowed, worker) || common.Value(c.WorkerCount) < 1 ||
			common.Value(c.WorkerCount) > 1000000 ||
			c.WorkerConfiguration == nil {
			return awserror.Validation(
				"initialCapacity must specify valid worker types, positive counts and workerConfiguration.",
			)
		}

		w := c.WorkerConfiguration
		if err := checkResources(w.Cpu, w.Memory, w.Disk); err != nil {
			return err
		}

		if w.DiskType != nil && !slices.Contains([]string{"STANDARD", "SHUFFLE_OPTIMIZED"}, *w.DiskType) {
			return awserror.Validation("Invalid diskType.")
		}
	}

	if a.MaximumCapacity != nil {
		c := a.MaximumCapacity
		if err := checkResources(c.Cpu, c.Memory, c.Disk); err != nil {
			return err
		}
	}

	if a.NetworkConfiguration != nil {
		n := a.NetworkConfiguration
		if len(n.SubnetIds) > 16 || len(n.SecurityGroupIds) > 5 {
			return awserror.Validation("Too many subnets or security groups.")
		}

		for _, id := range n.SubnetIds {
			if !strings.HasPrefix(id, "subnet-") || len(id) <= 7 {
				return awserror.Validation("Invalid subnet ID.")
			}
		}

		for _, id := range n.SecurityGroupIds {
			if !strings.HasPrefix(id, "sg-") || len(id) <= 3 {
				return awserror.Validation("Invalid security group ID.")
			}
		}
	}

	return nil
}

// tokenRecord keeps the exact original response even when the resource is later changed or removed.
type tokenRecord[T any] struct {
	Output T
	Hash   string
}

func tokenKey(scope request.Scope, operation string, token *string) (string, error) {
	v := common.Value(token)
	if !clientTokenPattern.MatchString(v) {
		return "", awserror.Validation("clientToken must contain 1 to 64 characters.")
	}

	return scope.Key(serviceName, "token", operation+"/"+fmt.Sprintf("%x", sha256.Sum256([]byte(v)))), nil
}

func requestHash(in any) (string, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func replay[T any](tx storage.ReadTx, k, hash string) (output T, found bool, err error) {
	var zero T
	r, found, err := storage.Read[tokenRecord[T]](tx, k)
	if err != nil {
		return zero, false, err
	}

	if !found {
		return zero, false, nil
	}

	if r.Hash != hash {
		return zero, false, conflict("clientToken was already used with different parameters.")
	}

	return r.Output, true, nil
}

// applicationTransaction centralizes scoped application updates and timestamp changes.
func (s *Service) applicationTransaction(
	ctx context.Context,
	id *string,
	change func(storage.WriteTx, request.Scope, *api.Application) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	var out *api.Application
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		out, e = readApplication(tx, scope, id)
		if e != nil {
			return e
		}

		if e := change(tx, scope, out); e != nil {
			return e
		}

		out.UpdatedAt = common.SecondsMilli(time.Now())
		return storage.Write(tx, appKey(scope, common.Value(id)), out)
	})
	return err
}
