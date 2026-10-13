// Package inithooks runs the user's init scripts at four stages of TinyStack's
// life, the way LocalStack does, and reports how they went. A stage reads the
// files of <dir>/<stage>.d in lexical order and runs them one at a time.
package inithooks

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/background"
)

// Stage is a point in TinyStack's life at which scripts run.
type Stage string

// The stages, in the order they happen. Boot comes before the storage opens and
// the services are built, start after they are built but before anything
// listens, ready once the listeners accept requests, and shutdown on a clean
// shutdown, before the listeners and the services stop.
const (
	Boot     Stage = "boot"
	Start    Stage = "start"
	Ready    Stage = "ready"
	Shutdown Stage = "shutdown"
)

// Stages lists every stage in order.
var Stages = []Stage{Boot, Start, Ready, Shutdown}

// State is how a stage or a script is going, named as LocalStack names them.
type State string

const (
	Unknown    State = "UNKNOWN"
	Running    State = "RUNNING"
	Successful State = "SUCCESSFUL"
	Failed     State = "ERROR"
)

// killDelay is how long a script's output pipes may stay open after the script
// is killed, which grandchildren that inherited them can prolong.
const killDelay = 5 * time.Second

// Script is one script's progress. Stage is upper case, as in LocalStack.
type Script struct {
	ExitCode *int   `json:"exit_code,omitempty"`
	Stage    string `json:"stage"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	State    State  `json:"state"`
	Error    string `json:"error,omitempty"`
}

// Status is every stage's progress: Completed is whether each stage finished,
// successfully or not.
type Status struct {
	Completed map[string]bool  `json:"completed"`
	Stages    map[string]State `json:"stages"`
	Scripts   []Script         `json:"scripts"`
}

// StageStatus is one stage's progress.
type StageStatus struct {
	Stage     string   `json:"stage"`
	State     State    `json:"state"`
	Scripts   []Script `json:"scripts"`
	Completed bool     `json:"completed"`
}

// Runner runs the scripts and remembers how they went.
type Runner struct {
	stages    map[Stage]*progress
	logger    *slog.Logger
	bg        *background.Group
	endpoint  string
	region    string
	accessKey string
	secret    string
	dirs      []string
	// run keeps stages from overlapping; mu guards the rest.
	run sync.Mutex
	mu  sync.Mutex
}

// progress is a stage's state and its scripts.
type progress struct {
	state   State
	scripts []Script
}

// task is a script that is about to run.
type task struct {
	name string
	path string
	argv []string
}

// New returns a runner for the directories the configuration names. Scripts
// reach TinyStack at the configured address, until UseEndpoint says otherwise.
//
//nolint:gocritic // Configuration is passed by value like config.Load returns it.
func New(cfg config.Config, logger *slog.Logger) *Runner {
	r := &Runner{
		logger:    logger,
		bg:        background.New(),
		stages:    map[Stage]*progress{},
		endpoint:  "http://" + net.JoinHostPort(reachable(cfg.Server.Host), strconv.Itoa(cfg.Server.Port)),
		region:    cfg.Defaults.Region,
		accessKey: "test",
		secret:    "test",
	}
	// With verify mode, scripts sign with the first configured key.
	if keys := slices.Sorted(maps.Keys(cfg.Identity.SecretKeys)); len(keys) > 0 {
		r.accessKey, r.secret = keys[0], cfg.Identity.SecretKeys[keys[0]]
	}

	for _, dir := range []string{cfg.Init.Dir, cfg.Init.LocalStackDir} {
		if dir = strings.TrimSpace(dir); dir != "" && !slices.Contains(r.dirs, filepath.Clean(dir)) {
			r.dirs = append(r.dirs, filepath.Clean(dir))
		}
	}

	for _, stage := range Stages {
		r.stages[stage] = &progress{state: Unknown}
	}

	return r
}

// reachable turns a listen host into one a client can dial.
func reachable(host string) string {
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return "127.0.0.1"
	}

	return host
}

// UseEndpoint sets the URL scripts get as AWS_ENDPOINT_URL.
func (r *Runner) UseEndpoint(endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoint = endpoint
}

// Stop cancels the stages Go started and waits for them. Run is not affected.
func (r *Runner) Stop() { r.bg.Stop() }

// Go runs stage in the background, until Stop, so that the caller can carry on
// serving, as the ready stage needs.
func (r *Runner) Go(stage Stage) {
	r.bg.Go(func(ctx context.Context) { r.Run(ctx, stage) })
}

// Run runs the scripts of stage one after another, waiting for any stage that
// is running, and returns how the stage went. A script that fails is logged and
// recorded, and the stage carries on; cancelling ctx stops it, and the scripts
// that did not run count as failed.
func (r *Runner) Run(ctx context.Context, stage Stage) State {
	r.run.Lock()
	defer r.run.Unlock()
	tasks := r.discover(stage)
	scripts := make([]Script, len(tasks))
	for i, t := range tasks {
		scripts[i] = Script{Stage: strings.ToUpper(string(stage)), Name: t.name, Path: t.path, State: Unknown}
	}

	r.set(stage, Running, scripts)
	log := r.logger.With("stage", string(stage))
	log.Info("init stage started", "scripts", len(tasks))
	state := Successful
	for i, t := range tasks {
		if ctx.Err() != nil {
			scripts[i].State, scripts[i].Error = Failed, "the stage was cancelled before the script ran"
			state = Failed
			r.set(stage, Running, scripts)
			continue
		}

		scripts[i].State = Running
		r.set(stage, Running, scripts)
		code, err := r.exec(ctx, stage, &t)
		scripts[i].ExitCode = code
		scripts[i].State = Successful
		if err != nil {
			scripts[i].State, scripts[i].Error = Failed, err.Error()
			state = Failed
			log.Error("init script failed", "script", t.name, "error", err)
		}

		r.set(stage, Running, scripts)
	}

	r.set(stage, state, scripts)
	log.Info("init stage finished", "state", string(state))
	return state
}

// discover lists the scripts of stage: TinyStack's directory first, then
// LocalStack's, each in lexical order. Other files are skipped with a log line.
func (r *Runner) discover(stage Stage) []task {
	var tasks []task
	for _, root := range r.dirs {
		// Scripts run in their own directory, so their command paths must be absolute.
		dir, err := filepath.Abs(filepath.Join(root, string(stage)+".d"))
		if err != nil {
			r.logger.Warn("resolve init directory", "dir", root, "error", err)
			continue
		}

		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			r.logger.Warn("read init directory", "dir", dir, "error", err)
			continue
		}

		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			// Stat follows symbolic links, which mounted script directories use.
			info, statErr := os.Stat(path)
			if statErr != nil || info.IsDir() {
				continue
			}

			switch ext := filepath.Ext(path); {
			case info.Mode()&0o111 != 0:
				tasks = append(tasks, task{name: entry.Name(), path: path, argv: []string{path}})
			case ext == ".sh":
				tasks = append(tasks, task{name: entry.Name(), path: path, argv: []string{"sh", path}})
			case ext == ".py":
				tasks = append(tasks, task{name: entry.Name(), path: path, argv: []string{"python3", path}})
			default:
				r.logger.Info("skipping init file that is not executable, .sh, or .py", "path", path)
			}
		}
	}

	return tasks
}

// exec runs one script, logging its output, and returns its exit code if it
// ran to the end.
func (r *Runner) exec(ctx context.Context, stage Stage, t *task) (*int, error) {
	// #nosec G204 -- the scripts are the user's own, from directories their configuration names.
	cmd := exec.CommandContext(ctx, t.argv[0], t.argv[1:]...)
	cmd.Dir = filepath.Dir(t.path)
	cmd.Env = r.environment()
	cmd.WaitDelay = killDelay
	log := r.logger.With("stage", string(stage), "script", t.name)
	stdout := &lineLog{emit: func(line string) { log.Info(line) }}
	stderr := &lineLog{emit: func(line string) { log.Warn(line) }}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	if err == nil {
		return new(0), nil
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() >= 0 {
		return new(exit.ExitCode()), err
	}

	return nil, err
}

// environment is the process's environment plus the endpoint, and defaults for
// the credentials and Region that the environment does not set.
func (r *Runner) environment() []string {
	r.mu.Lock()
	endpoint := r.endpoint
	r.mu.Unlock()
	env := append(os.Environ(), "AWS_ENDPOINT_URL="+endpoint)
	for _, d := range [][2]string{
		{"AWS_DEFAULT_REGION", r.region},
		{"AWS_REGION", r.region},
		{"AWS_ACCESS_KEY_ID", r.accessKey},
		{"AWS_SECRET_ACCESS_KEY", r.secret},
	} {
		if _, set := os.LookupEnv(d[0]); !set {
			env = append(env, d[0]+"="+d[1])
		}
	}

	return env
}

func (r *Runner) set(stage Stage, state State, scripts []Script) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages[stage] = &progress{state: state, scripts: slices.Clone(scripts)}
}

// Status reports every stage and script.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Status{Completed: map[string]bool{}, Stages: map[string]State{}, Scripts: []Script{}}
	for _, stage := range Stages {
		p := r.stages[stage]
		name := strings.ToUpper(string(stage))
		out.Completed[name] = finished(p.state)
		out.Stages[name] = p.state
		out.Scripts = append(out.Scripts, p.scripts...)
	}

	return out
}

// StageStatus reports one stage.
func (r *Runner) StageStatus(stage Stage) StageStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.stages[stage]
	return StageStatus{
		Stage:     strings.ToUpper(string(stage)),
		State:     p.state,
		Completed: finished(p.state),
		Scripts:   append([]Script{}, p.scripts...),
	}
}

func finished(state State) bool { return state == Successful || state == Failed }

// lineLog turns what a script writes into log lines.
type lineLog struct {
	emit func(string)
	buf  bytes.Buffer
}

// maxLine bounds a log line, so a script that never ends one cannot grow the
// buffer without limit.
const maxLine = 64 << 10

func (l *lineLog) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			// Not a whole line yet: put it back.
			rest := line
			l.buf.Reset()
			l.buf.WriteString(rest)
			break
		}

		l.emit(strings.TrimRight(line, "\r\n"))
	}

	if l.buf.Len() >= maxLine {
		l.flush()
	}

	return len(p), nil
}

func (l *lineLog) flush() {
	if l.buf.Len() > 0 {
		l.emit(strings.TrimRight(l.buf.String(), "\r\n"))
		l.buf.Reset()
	}
}
