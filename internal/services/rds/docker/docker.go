// Package docker runs RDS engines on loopback ports with named data volumes.
package docker

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/rds"
)

const (
	rootUser    = "root"
	execCommand = "exec"
)

var _ rds.Runtime = (*Runtime)(nil)

type Runtime struct {
	specs  map[string]*rds.Spec
	mu     sync.Mutex
	closed bool
}

func New() *Runtime { return &Runtime{specs: map[string]*rds.Spec{}} }

// Resources finds containers and retained volumes left by earlier runtimes.
func (r *Runtime) Resources(ctx context.Context) ([]*rds.Spec, error) {
	containers, err := dockercli.ListLabeled(ctx, "tinystack.rds")
	if err != nil {
		return nil, err
	}

	out, err := dockercli.Run(ctx, nil, "volume", "ls", "--filter", "label=tinystack.rds",
		"--format", `{{.Name}} {{.Label "tinystack.rds"}}`)
	if err != nil {
		return nil, err
	}

	specs := make([]*rds.Spec, 0, len(containers))
	for id, arn := range containers {
		specs = append(specs, &rds.Spec{Name: id, ARN: arn})
	}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			specs = append(specs, &rds.Spec{Volume: fields[0], ARN: fields[1]})
		}
	}

	return specs, nil
}

func (r *Runtime) Ensure(ctx context.Context, spec *rds.Spec) (rds.Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return rds.Started{}, fmt.Errorf("runtime is closed")
	}

	r.specs[spec.Name] = spec
	id, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.Id}}", spec.Name)
	if err != nil && !dockercli.Gone(err) {
		return rds.Started{}, err
	}

	if err == nil {
		if _, err = dockercli.Run(ctx, nil, "start", spec.Name); err != nil {
			return rds.Started{}, err
		}
	} else {
		if _, err = dockercli.Run(
			ctx,
			nil,
			"volume",
			"create",
			"--label",
			"tinystack.rds="+spec.ARN,
			spec.Volume,
		); err != nil {
			return rds.Started{}, err
		}

		args := []string{
			"run", "-d", "--name", spec.Name, "--label", "tinystack.rds=" + spec.ARN,
			"-p", fmt.Sprintf("127.0.0.1::%d/tcp", spec.Port),
		}

		path := "/var/lib/mysql"
		if spec.Engine == rds.EnginePostgres {
			path = "/var/lib/postgresql/data"
			args = append(args, "-e", "POSTGRES_USER="+spec.Username, "-e", "POSTGRES_PASSWORD="+spec.Password,
				"-e", "POSTGRES_DB="+spec.Database)
		} else {
			prefix := "MYSQL_"
			if spec.Engine == rds.EngineMariaDB {
				prefix = "MARIADB_"
			}

			args = append(args, "-e", prefix+"ROOT_PASSWORD="+spec.RootPassword,
				"-e", prefix+"DATABASE="+spec.Database, "-e", prefix+"ROOT_HOST=%")
			if spec.Username != rootUser {
				args = append(args, "-e", prefix+"USER="+spec.Username, "-e", prefix+"PASSWORD="+spec.Password)
			}
		}

		args = append(args, "-v", spec.Volume+":"+path, "--", spec.Image)
		id, err = dockercli.Run(ctx, nil, args...)
		if err != nil {
			return rds.Started{}, err
		}
	}

	port, err := dockercli.HostPort(ctx, spec.Name, spec.Port)
	if err != nil {
		return rds.Started{}, err
	}

	return rds.Started{ID: id, Port: port}, nil
}

func (r *Runtime) Probe(ctx context.Context, spec *rds.Spec, started rds.Started) error {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.FormatInt(started.Port, 10)))
	if err != nil {
		return err
	}

	if closeErr := conn.Close(); closeErr != nil {
		return closeErr
	}

	args := []string{execCommand, spec.Name, "pg_isready", "-U", spec.Username, "-d", spec.Database}
	if spec.Engine != rds.EnginePostgres {
		command := "mysqladmin"
		if spec.Engine == rds.EngineMariaDB {
			command = "mariadb-admin"
		}

		args = []string{
			execCommand, "-e", "MYSQL_PWD=" + spec.Password, spec.Name, command, "ping", "--silent", "-h", "127.0.0.1",
			"-u", spec.Username,
		}
	}

	_, err = dockercli.Run(ctx, nil, args...)
	return err
}

func (r *Runtime) Action(ctx context.Context, spec *rds.Spec, action string) error {
	r.mu.Lock()
	r.specs[spec.Name] = spec
	r.mu.Unlock()
	_, err := dockercli.Run(ctx, nil, action, spec.Name)
	return err
}

func (r *Runtime) Password(ctx context.Context, spec *rds.Spec, password string) error {
	escaped := strings.ReplaceAll(password, "'", "''")
	args := []string{
		execCommand,
		"-i",
		spec.Name,
		"psql",
		"-v",
		"ON_ERROR_STOP=1",
		"-U",
		spec.Username,
		"-d",
		spec.Database,
	}

	sql := fmt.Sprintf("ALTER ROLE %q WITH PASSWORD '%s';", spec.Username, escaped)
	if spec.Engine != rds.EnginePostgres {
		// NO_BACKSLASH_ESCAPES makes literal quoting independent of the server's default SQL mode.
		command := "mysql"
		if spec.Engine == rds.EngineMariaDB {
			command = rds.EngineMariaDB
		}

		args = []string{execCommand, "-i", "-e", "MYSQL_PWD=" + spec.RootPassword, spec.Name, command, "-u", rootUser}
		sql = fmt.Sprintf(
			"SET sql_mode='NO_BACKSLASH_ESCAPES'; ALTER USER '%s'@'%%' IDENTIFIED BY '%s';",
			spec.Username,
			escaped,
		)
		if spec.Username == rootUser {
			sql = fmt.Sprintf(
				"SET sql_mode='NO_BACKSLASH_ESCAPES'; "+
					"ALTER USER IF EXISTS 'root'@'%%' IDENTIFIED BY '%s'; "+
					"ALTER USER 'root'@'localhost' IDENTIFIED BY '%s';",
				escaped, escaped,
			)
		}
	}

	_, err := dockercli.Run(ctx, strings.NewReader(sql), args...)
	return err
}

func (r *Runtime) Remove(ctx context.Context, spec *rds.Spec, volume bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if spec.Name != "" {
		if _, err := dockercli.Run(ctx, nil, "rm", "-f", spec.Name); err != nil && !dockercli.Gone(err) {
			return err
		}
	}

	if volume && spec.Volume != "" {
		if _, err := dockercli.Run(ctx, nil, "volume", "rm", spec.Volume); err != nil && !dockercli.Gone(err) {
			return err
		}
	}

	r.mu.Lock()
	delete(r.specs, spec.Name)
	r.mu.Unlock()
	return nil
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	specs := slices.Collect(maps.Values(r.specs))
	r.mu.Unlock()
	for _, spec := range specs {
		_ = r.Remove(context.Background(), spec, false)
	}
}
