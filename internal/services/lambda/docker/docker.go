// Package docker runs Lambda functions in containers through the docker CLI.
//
// Zip functions run in the AWS base image of their runtime and Image functions
// in their own image; both images contain the Lambda Runtime Interface
// Emulator, which serves invocations on port 8080. Code is copied into the
// container with "docker cp" instead of a bind mount, so it works with remote
// and VM-backed daemons (Colima, Docker Desktop) alike. Warm containers are
// pooled per function and reused until the function changes or is deleted.
package docker

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

const (
	// initTimeout bounds how long a started container may take to listen.
	initTimeout = 10 * time.Second
	// grace is added to the function timeout, which the emulator enforces.
	grace = 3 * time.Second
	// logWait bounds how long an invocation waits for its log to arrive.
	logWait     = 500 * time.Millisecond
	invocations = "/2015-03-31/functions/function/invocations"
)

var _ lambda.Runtime = (*Runtime)(nil)

// Runtime runs functions in Docker containers.
type Runtime struct {
	pools  map[string]*pool
	client *http.Client
	mu     sync.Mutex
	closed bool
}

// pool holds the idle containers of one function revision.
type pool struct {
	busy     map[*container]bool
	revision string
	idle     []*container
}

type container struct {
	// since is the daemon time of the last log line an invocation took.
	since    time.Time
	id       string
	endpoint string
}

func New() *Runtime {
	return &Runtime{pools: map[string]*pool{}, client: &http.Client{}}
}

// remove deletes a container. It outlives the caller's cancellation, since a
// container must be removed even when its invocation was abandoned.
func remove(ctx context.Context, c *container) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = dockercli.Run(ctx, nil, "rm", "-f", c.id)
}

// acquire takes an idle container of the function's current revision or
// starts a new one. Containers of an older revision are removed.
func (r *Runtime) acquire(ctx context.Context, fn *lambda.Function) (*container, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("runtime is closed")
	}

	p := r.pools[fn.ARN]
	if p == nil || p.revision != fn.Revision {
		if p != nil {
			for _, c := range p.idle {
				go remove(ctx, c)
			}
		}

		p = &pool{revision: fn.Revision, busy: map[*container]bool{}}
		r.pools[fn.ARN] = p
	}

	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.busy[c] = true
		r.mu.Unlock()
		return c, nil
	}

	r.mu.Unlock()
	c, err := start(ctx, r.client, fn)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.pools[fn.ARN] != p {
		go remove(ctx, c)
		return nil, errors.New("function changed while its container started")
	}

	p.busy[c] = true
	return c, nil
}

// release returns a container to its pool, or removes it when it is broken or
// its function changed.
func (r *Runtime) release(ctx context.Context, fn *lambda.Function, c *container, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pools[fn.ARN]
	if p != nil && p.busy[c] {
		delete(p.busy, c)
		if healthy && !r.closed {
			p.idle = append(p.idle, c)
			return
		}
	}

	go remove(ctx, c)
}

// Release removes the containers of the function with arn.
func (r *Runtime) Release(arn string) {
	r.mu.Lock()
	p := r.pools[arn]
	delete(r.pools, arn)
	r.mu.Unlock()
	if p != nil {
		for _, c := range p.idle {
			go remove(context.Background(), c)
		}
		// Busy containers are removed when their invocation returns.
	}
}

// Close removes every container, refuses new ones, and waits for the removals.
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.Reset()
}

// Reset removes every container and waits for the removals. Unlike Close, the
// runtime keeps serving, so functions start fresh containers on their next
// invocation.
func (r *Runtime) Reset() {
	r.mu.Lock()
	var all []*container
	for _, p := range r.pools {
		all = append(all, p.idle...)
		for c := range p.busy {
			all = append(all, c)
		}
	}

	r.pools = map[string]*pool{}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range all {
		wg.Go(func() { remove(context.Background(), c) })
	}

	wg.Wait()
}

// start creates a container for fn, copies its code, and waits until the
// emulator listens.
func start(ctx context.Context, client *http.Client, fn *lambda.Function) (*container, error) {
	image := fn.ImageURI
	args := []string{"create", "--rm", "-p", "127.0.0.1::8080", "--label", "tinystack.function=" + fn.ARN}
	switch fn.Architecture {
	case "arm64":
		args = append(args, "--platform", "linux/arm64")
	case "x86_64":
		args = append(args, "--platform", "linux/amd64")
	}

	for k, v := range fn.Environment {
		args = append(args, "-e", k+"="+v)
	}

	var code io.Reader
	var layers []io.Reader
	if fn.PackageType == "Zip" {
		var ok bool
		if image, ok = lambda.Image(fn.Runtime); !ok {
			return nil, fmt.Errorf("runtime %s is not supported", fn.Runtime)
		}

		archive, err := tarball(fn.Code)
		if err != nil {
			return nil, err
		}

		code = archive
		for _, layer := range fn.Layers {
			if archive, err = tarball(layer); err != nil {
				return nil, err
			}

			layers = append(layers, archive)
		}
	}

	args = append(args, image)
	if fn.PackageType == "Zip" {
		args = append(args, fn.Handler)
	}

	id, err := dockercli.Run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}

	c := &container{id: id}
	if err = c.boot(ctx, client, code, layers); err != nil {
		remove(ctx, c)
		return nil, err
	}

	return c, nil
}

// boot copies the layers into /opt, in order, and the code into /var/task,
// and starts the container.
func (c *container) boot(ctx context.Context, client *http.Client, code io.Reader, layers []io.Reader) error {
	for _, layer := range layers {
		if _, err := dockercli.Run(ctx, layer, "cp", "-", c.id+":/opt"); err != nil {
			return err
		}
	}

	if code != nil {
		if _, err := dockercli.Run(ctx, code, "cp", "-", c.id+":/var/task"); err != nil {
			return err
		}
	}

	if _, err := dockercli.Run(ctx, nil, "start", c.id); err != nil {
		return err
	}

	ports, err := dockercli.Run(ctx, nil, "port", c.id, "8080/tcp")
	if err != nil {
		return err
	}

	// "docker port" may list an IPv4 and an IPv6 binding; use the first.
	c.endpoint, _, _ = strings.Cut(ports, "\n")
	// Port forwarders accept connections before the emulator listens, so only
	// an HTTP answer (any status) shows the container is ready.
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	for {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.endpoint+"/", http.NoBody)
		if reqErr != nil {
			return reqErr
		}

		resp, probeErr := client.Do(req)
		if probeErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("function container did not start listening: %w", probeErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// tarball converts a deployment package into the tar stream "docker cp" reads.
func tarball(code []byte) (io.Reader, error) {
	archive, err := zip.NewReader(bytes.NewReader(code), int64(len(code)))
	if err != nil {
		return nil, fmt.Errorf("read deployment package: %w", err)
	}

	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, f := range archive.File {
		name := path.Clean(f.Name)
		if strings.HasPrefix(name, "../") || name == ".." || path.IsAbs(name) {
			return nil, fmt.Errorf("deployment package entry %s escapes the task root", f.Name)
		}

		mode := int64(f.Mode().Perm())
		if f.FileInfo().IsDir() {
			if mode == 0 {
				mode = 0o755
			}

			header := &tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: mode}
			if headerErr := w.WriteHeader(header); headerErr != nil {
				return nil, headerErr
			}

			continue
		}
		// Archives made without Unix permissions get readable files.
		if mode == 0 {
			mode = 0o644
		}

		if f.UncompressedSize64 > math.MaxInt64 {
			return nil, fmt.Errorf("deployment package entry %s is too large", f.Name)
		}

		header := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(f.UncompressedSize64)}
		if headerErr := w.WriteHeader(header); headerErr != nil {
			return nil, headerErr
		}

		rc, openErr := f.Open()
		if openErr != nil {
			return nil, openErr
		}

		_, err = io.Copy(w, rc) // #nosec G110 -- the service bounds the unzipped size.
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
	}

	return &out, w.Close()
}

// Invoke runs one invocation in a warm or new container.
func (r *Runtime) Invoke(ctx context.Context, fn *lambda.Function, payload []byte) (lambda.Result, error) {
	c, err := r.acquire(ctx, fn)
	if err != nil {
		return lambda.Result{}, err
	}

	result, healthy, err := r.post(ctx, fn, c, payload)
	if err == nil {
		result.Log = c.output(ctx)
	}

	r.release(ctx, fn, c, healthy)
	return result, err
}

// output returns what the function wrote since the container's previous
// invocation. The emulator writes its REPORT line after it answers, so output
// polls "docker logs" for it for up to logWait. The emulator's own lines are
// dropped, since Lambda writes its own START, END, and REPORT lines.
func (c *container) output(ctx context.Context) string {
	deadline := time.Now().Add(logWait)
	for {
		lines, last, done := c.logs(ctx)
		if done || time.Now().After(deadline) {
			if !last.IsZero() {
				c.since = last
			}

			return strings.Join(lines, "\n")
		}

		select {
		case <-ctx.Done():
			return strings.Join(lines, "\n")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// logs reads the container's stdout and stderr after c.since, in time order.
// It returns the function's lines, the time of the last line read, and whether
// the emulator's REPORT line has arrived.
func (c *container) logs(ctx context.Context) (lines []string, last time.Time, done bool) {
	args := []string{"logs", "--timestamps"}
	if !c.since.IsZero() {
		args = append(args, "--since", c.since.Format(time.RFC3339Nano))
	}

	// #nosec G204 -- the container ID comes from docker create.
	cmd := exec.CommandContext(ctx, "docker", append(args, c.id)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if cmd.Run() != nil {
		return nil, time.Time{}, false
	}

	type line struct {
		at   time.Time
		text string
	}

	var all []line
	for _, out := range []string{stdout.String(), stderr.String()} {
		for raw := range strings.SplitSeq(out, "\n") {
			stamp, text, _ := strings.Cut(raw, " ")
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err == nil && at.After(c.since) {
				all = append(all, line{at: at, text: strings.TrimRight(text, "\r")})
			}
		}
	}

	slices.SortStableFunc(all, func(a, b line) int { return a.at.Compare(b.at) })
	for _, l := range all {
		last = l.at
		switch {
		case strings.HasPrefix(l.text, "REPORT RequestId:"):
			done = true
		case strings.HasPrefix(l.text, "START RequestId:"), strings.HasPrefix(l.text, "END RequestId:"),
			strings.Contains(l.text, "] (rapid) "):
		default:
			lines = append(lines, l.text)
		}
	}

	return lines, last, done
}

// post sends the payload to the emulator and classifies its answer: 502 means
// the runtime crashed, a timeout is reported as text, and a handled error is a
// JSON document with errorType and errorMessage.
func (r *Runtime) post(
	ctx context.Context,
	fn *lambda.Function,
	c *container,
	payload []byte,
) (result lambda.Result, healthy bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, fn.Timeout+grace)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://"+c.endpoint+invocations,
		bytes.NewReader(payload),
	)
	if err != nil {
		return result, false, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return timedOut(fn), false, nil
		}

		return result, false, err
	}

	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, false, err
	}

	switch {
	case resp.StatusCode == http.StatusBadGateway:
		return lambda.Result{Payload: body, Error: true}, true, nil
	case resp.StatusCode != http.StatusOK:
		return result, false, fmt.Errorf("runtime emulator answered %s: %s", resp.Status, body)
	case bytes.HasPrefix(body, []byte("Task timed out after")):
		return timedOut(fn), true, nil
	case isError(body):
		return lambda.Result{Payload: body, Error: true}, true, nil
	default:
		return lambda.Result{Payload: body}, true, nil
	}
}

func timedOut(fn *lambda.Function) lambda.Result {
	body, _ := json.Marshal(map[string]string{
		"errorType":    "Sandbox.Timedout",
		"errorMessage": fmt.Sprintf("Task timed out after %.2f seconds", fn.Timeout.Seconds()),
	})
	return lambda.Result{Payload: body, Error: true}
}

// isError reports whether a response is the error document runtimes post when
// a handler fails.
func isError(body []byte) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return false
	}

	var kind, message string
	return json.Unmarshal(doc["errorType"], &kind) == nil && kind != "" &&
		json.Unmarshal(doc["errorMessage"], &message) == nil
}
