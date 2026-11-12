// Package docker runs RDS engines on loopback ports with named data volumes.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
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
	loopback    = "127.0.0.1"
)

var _ rds.Runtime = (*Runtime)(nil)

type Runtime struct {
	specs  map[string]*rds.Spec
	mu     sync.Mutex
	closed bool
}

func New() *Runtime { return &Runtime{specs: map[string]*rds.Spec{}} }

// Resources finds containers and retained volumes left by earlier runtimes.
func (r *Runtime) Resources(ctx context.Context, owner string) ([]*rds.Spec, error) {
	containers, volumes, err := dockercli.ListOwned(ctx, "tinystack.rds", "tinystack.rds.owner", owner)
	if err != nil {
		return nil, err
	}

	specs := make([]*rds.Spec, 0, len(containers)+len(volumes))
	for id, arn := range containers {
		specs = append(specs, &rds.Spec{Name: id, ARN: arn})
	}

	for name, arn := range volumes {
		specs = append(specs, &rds.Spec{Volume: name, ARN: arn})
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
			"--label", "tinystack.rds.owner="+spec.Owner,
			spec.Volume,
		); err != nil {
			return rds.Started{}, err
		}

		args := []string{
			"run", "-d", "--name", spec.Name, "--label", "tinystack.rds=" + spec.ARN,
			"--label", "tinystack.rds.owner=" + spec.Owner,
			"-p", fmt.Sprintf("127.0.0.1::%d/tcp", spec.Port),
		}

		path := "/var/lib/mysql"
		switch spec.Engine {
		case rds.EnginePostgres:
			path = "/var/lib/postgresql/data"
			args = append(args, "-e", "POSTGRES_USER="+spec.Username, "-e", "POSTGRES_PASSWORD="+spec.Password,
				"-e", "POSTGRES_DB="+spec.Database)
		case rds.EngineNeptune:
			// The stock TinkerGraph is in-memory. This owned volume holds no graph data.
			path = "/data"
		case rds.EngineDocDB:
			path = "/data/db"
			args = append(args, "--tmpfs", "/data/configdb", "-e", "MONGO_INITDB_ROOT_USERNAME="+spec.Username,
				"-e", "MONGO_INITDB_ROOT_PASSWORD="+spec.Password)
		default:
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

		args = append(args, "-v", spec.Volume+":"+path)
		if spec.Engine == rds.EngineNeptune {
			// Bound JIT startup work, including when Docker emulates the amd64-only 3.6.2 image.
			args = append(args, "-e", "JAVA_OPTIONS=-XX:TieredStopAtLevel=1",
				"--entrypoint", "sh", "--", spec.Image, "-c", gremlinStartup)
		} else {
			args = append(args, "--", spec.Image)
			args = append(args, parameterArguments(spec)...)
		}

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
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(loopback, strconv.FormatInt(started.Port, 10)))
	if err != nil {
		return err
	}

	if closeErr := conn.Close(); closeErr != nil {
		return closeErr
	}

	if spec.Engine == rds.EngineNeptune {
		return probeGremlin(ctx, started.Port)
	}

	// Probe over TCP: the image's init server listens only on its socket, and Docker's port proxy accepts
	// connections before anything in the container listens.
	args := []string{
		execCommand, spec.Name, "pg_isready", "-h", loopback, "-U", spec.Username, "-d", spec.Database,
	}

	if spec.Engine != rds.EnginePostgres && spec.Engine != rds.EngineDocDB {
		command := "mysqladmin"
		if spec.Engine == rds.EngineMariaDB {
			command = "mariadb-admin"
		}

		args = []string{
			execCommand, "-e", "MYSQL_PWD=" + spec.Password, spec.Name, command, "ping", "--silent", "-h", loopback,
			"-u", spec.Username,
		}
	}

	if spec.Engine == rds.EngineDocDB {
		args = mongoCommand(spec, "if (db.adminCommand({ping:1}).ok !== 1) { quit(1); }")
	}

	_, err = dockercli.Run(ctx, nil, args...)
	return err
}

func (r *Runtime) Action(ctx context.Context, spec *rds.Spec, action string) error {
	if action == "restart" || action == "stop" {
		// Shut the engine down cleanly, then recreate the container so startup arguments change; the volume stays.
		if _, err := dockercli.Run(ctx, nil, "stop", spec.Name); err != nil && !dockercli.Gone(err) {
			return err
		}

		return r.Remove(ctx, spec, false)
	}

	r.mu.Lock()
	r.specs[spec.Name] = spec
	r.mu.Unlock()
	_, err := dockercli.Run(ctx, nil, action, spec.Name)
	return err
}

func (r *Runtime) Password(ctx context.Context, spec *rds.Spec, password string) error {
	if spec.Engine == rds.EngineDocDB {
		encoded, err := json.Marshal(password)
		if err != nil {
			return err
		}

		_, err = dockercli.Run(ctx, nil, mongoCommand(spec, "db.changeUserPassword("+
			strconv.Quote(spec.Username)+", "+string(encoded)+")")...)
		return err
	}

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

func mongoCommand(spec *rds.Spec, script string) []string {
	shell := "mongo"
	if spec.Image == "mongo:5.0" {
		shell = "mongosh"
	}

	return []string{
		execCommand, spec.Name, shell, "--quiet", "--host", loopback,
		"--username", spec.Username, "--password", spec.Password, "--authenticationDatabase", "admin",
		"admin", "--eval", script,
	}
}

// Enable Gremlin HTTP alongside WebSocket; the image entrypoint sets the container's bind address.
const gremlinStartup = `sed -i 's/WebSocketChannelizer/WsAndHttpChannelizer/' conf/gremlin-server.yaml &&
exec /docker-entrypoint.sh conf/gremlin-server.yaml`

func probeGremlin(ctx context.Context, port int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/gremlin", port), strings.NewReader(`{"gremlin":"g.inject(1)"}`))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()
	var result struct{ Status struct{ Code int } }
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return err
	}

	if response.StatusCode != http.StatusOK || result.Status.Code != http.StatusOK {
		return fmt.Errorf("gremlin readiness failed: HTTP %d, Gremlin %d", response.StatusCode, result.Status.Code)
	}

	return nil
}

func parameterArguments(spec *rds.Spec) []string {
	if len(spec.Parameters) == 0 {
		return nil
	}

	args := []string{}
	switch spec.Engine {
	case rds.EnginePostgres:
		args = append(args, "postgres")
	case rds.EngineMariaDB:
		args = append(args, "mariadbd")
	default:
		args = append(args, "mysqld")
	}

	for _, name := range slices.Sorted(maps.Keys(spec.Parameters)) {
		value := spec.Parameters[name]
		if spec.Engine == rds.EnginePostgres {
			args = append(args, "-c", name+"="+value)
		} else {
			args = append(args, "--"+strings.ReplaceAll(name, "_", "-")+"="+value)
		}
	}

	return args
}

// ApplyParameters uses root credentials for MySQL's global variables. PostgreSQL
// startup -c settings take precedence over ALTER SYSTEM, so defer those to reboot.
func (r *Runtime) ApplyParameters(ctx context.Context, spec *rds.Spec, values map[string]string) error {
	if spec.Engine != rds.EngineMySQL && spec.Engine != rds.EngineMariaDB {
		return fmt.Errorf("engine requires a restart to replace startup parameters")
	}

	command := "mysql"
	if spec.Engine == rds.EngineMariaDB {
		command = rds.EngineMariaDB
	}

	var sql strings.Builder
	sql.WriteString("SET SESSION sql_mode='NO_BACKSLASH_ESCAPES';")
	for _, name := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(&sql, "SET GLOBAL `%s` = '%s';", name, strings.ReplaceAll(values[name], "'", "''"))
	}

	_, err := dockercli.Run(
		ctx,
		strings.NewReader(sql.String()),
		execCommand,
		"-i",
		"-e",
		"MYSQL_PWD="+spec.RootPassword,
		spec.Name,
		command,
		"-u",
		rootUser,
	)
	return err
}
