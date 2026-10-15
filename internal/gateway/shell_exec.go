//go:build console

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	shellStderr     = "stderr"
	shellTimeout    = 5 * time.Minute
	shellMissingCLI = "The AWS CLI is not installed on the TinyStack host; install AWS CLI v2. " +
		"See https://amjadjibon.github.io/tinystack/guides/cloudshell/."
)

// shellArgv implements quoting only: metacharacters and variables stay literal.
func shellArgv(line string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	for _, ch := range line {
		switch {
		case escaped:
			word.WriteRune(ch)
			escaped = false
		case ch == '\\' && quote != '\'':
			escaped, started = true, true
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
		case ch == '\'' || ch == '"':
			quote, started = ch, true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}

	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}

	if started {
		args = append(args, word.String())
	}

	if len(args) == 0 || args[0] != "aws" {
		return nil, errors.New("CloudShell commands must start with aws; use help, history or clear in the terminal")
	}

	for _, arg := range args[1:] {
		// Reject abbreviations too: argparse accepts unique prefixes of global options.
		name, _, _ := strings.Cut(arg, "=")
		for _, option := range []string{"--endpoint-url", "--profile", "--cli-auto-prompt"} {
			if strings.HasPrefix(name, "--") && strings.HasPrefix(option, name) {
				return nil, errors.New(
					"CloudShell controls --endpoint-url and --profile; interactive auto-prompt is unavailable",
				)
			}
		}
	}

	return args[1:], nil
}

func (s *Server) shellEnvironment(ctx context.Context, c *call, dir, binary string) ([]string, error) {
	key, secret, token := c.meta.AccountID, "test", ""
	if s.cfg.Identity.Mode == config.IdentityVerify {
		verified := c.credentials
		if verified == nil {
			arn := "arn:aws:iam::" + c.meta.AccountID + ":root"
			verified = &request.Credentials{
				AccountID: c.meta.AccountID,
				Principal: request.Principal{
					Kind: request.PrincipalRoot, Account: c.meta.AccountID,
					ARN: arn, PolicyARN: arn, ID: c.meta.AccountID, UserID: c.meta.AccountID,
				},
			}
		}

		if s.issueShell == nil {
			return nil, errors.New("CloudShell session issuer is unavailable")
		}

		issued, err := s.issueShell(ctx, verified)
		if err != nil {
			return nil, err
		}

		key, secret, token = *issued.AccessKeyId, *issued.SecretAccessKey, *issued.SessionToken
	}

	host := s.cfg.Server.Host
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(s.cfg.Server.Port))
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		return nil, err
	}

	return []string{
		"AWS_ACCESS_KEY_ID=" + key, "AWS_SECRET_ACCESS_KEY=" + secret, "AWS_SESSION_TOKEN=" + token,
		"AWS_REGION=" + c.meta.Region, "AWS_DEFAULT_REGION=" + c.meta.Region, "AWS_ENDPOINT_URL=" + endpoint,
		"AWS_PAGER=", "AWS_CLI_AUTO_PROMPT=off", "AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + empty, "AWS_SHARED_CREDENTIALS_FILE=" + empty, "HOME=" + dir,
		"PATH=" + filepath.Dir(binary) + ":/usr/bin:/bin", "LC_ALL=C.UTF-8",
	}, nil
}

type shellChunk struct {
	Exit   *int   `json:"exit,omitempty"`
	Stream string `json:"stream,omitempty"`
	Data   string `json:"data,omitempty"`
}

type shellStream struct {
	w         http.ResponseWriter
	cancel    context.CancelFunc
	mu        sync.Mutex
	remaining int
	capped    bool
}

type shellWriter struct {
	stream  *shellStream
	name    string
	pending []byte
}

func (w *shellWriter) Write(p []byte) (int, error) {
	s := w.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if len(p) > s.remaining {
		p = p[:s.remaining]
		s.capped = true
		s.cancel()
	}

	s.remaining -= len(p)
	w.pending = append(w.pending, p...)
	data := w.pending
	end := 0
	for end < len(data) && utf8.FullRune(data[end:]) {
		_, size := utf8.DecodeRune(data[end:])
		end += size
	}
	w.pending = append([]byte(nil), data[end:]...)
	if err := s.emit(w.name, data[:end]); err != nil {
		return 0, err
	}
	return n, nil
}

// emit requires the stream mutex; incomplete UTF-8 stays in the writer until its next chunk.
func (s *shellStream) emit(name string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.NewEncoder(s.w).Encode(shellChunk{Stream: name, Data: string(data)}); err != nil {
		s.cancel()
		return err
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.cancel()
		return err
	}
	return nil
}

func (w *shellWriter) finish() {
	w.stream.mu.Lock()
	defer w.stream.mu.Unlock()
	_ = w.stream.emit(w.name, w.pending)
	w.pending = nil
}

func shellProcess(ctx context.Context, binary string, args, env []string, dir string) *exec.Cmd {
	// #nosec G204 -- only the resolved AWS CLI/completer executes, without a shell, after console authentication.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env, cmd.Dir = env, dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	// A nil Stdin is connected to the null device by os/exec.
	return cmd
}

func (s *Server) runShell(w http.ResponseWriter, r *http.Request, c *call, in *shellRequest) {
	args, err := shellArgv(in.Command)
	if err != nil && !in.Complete {
		s.consoleFail(w, c, awserror.Validation("%s", err))
		return
	}

	binary, err := exec.LookPath("aws")
	if err != nil {
		if in.Complete {
			_ = json.NewEncoder(w).Encode(shellCandidates{Candidates: []string{}})
			return
		}

		s.consoleFail(w, c, &awserror.Error{Code: "AWSCLINotInstalled", Message: shellMissingCLI, StatusCode: 503})
		return
	}

	dir, err := os.MkdirTemp("", "tinystack-cloudshell-")
	if err != nil {
		s.consoleFail(w, c, err)
		return
	}

	defer func() { _ = os.RemoveAll(dir) }()
	env, err := s.shellEnvironment(r.Context(), c, dir, binary)
	if err != nil {
		s.consoleFail(w, c, err)
		return
	}

	if in.Complete {
		s.completeShell(w, r, in.Command, binary, env, dir)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), shellTimeout)
	defer cancel()
	stream := &shellStream{w: w, cancel: cancel, remaining: shellMaxOutput}
	cmd := shellProcess(ctx, binary, args, env, dir)
	stdout := &shellWriter{stream: stream, name: "stdout"}
	stderr := &shellWriter{stream: stream, name: shellStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	w.Header().Set("Content-Type", "application/x-ndjson")
	// The console normally has a 30-second write deadline; commands have their own longer bound.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(shellTimeout + time.Second))
	err = cmd.Run()
	stdout.finish()
	stderr.finish()
	exit := 0
	if err != nil {
		exit = 1
		if e, ok := errors.AsType[*exec.ExitError](err); ok {
			exit = e.ExitCode()
		}

		c.failure = awserror.New("CloudShellCommandFailed", err.Error())
	}

	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	if stream.capped {
		_ = json.NewEncoder(w).
			Encode(shellChunk{Stream: shellStderr, Data: fmt.Sprintf("\nOutput truncated at %d MiB.\n", shellMaxOutput>>20)})
	} else if err != nil && exit == 1 {
		_ = json.NewEncoder(w).Encode(shellChunk{Stream: shellStderr, Data: err.Error() + "\n"})
	}

	if ctx.Err() != nil && !stream.capped {
		_ = json.NewEncoder(w).Encode(shellChunk{Stream: shellStderr, Data: "Command canceled or timed out.\n"})
	}

	_ = json.NewEncoder(w).Encode(shellChunk{Exit: &exit})
	_ = http.NewResponseController(w).Flush()
}

func (s *Server) completeShell(w http.ResponseWriter, r *http.Request, line, binary string, env []string, dir string) {
	candidates := []string{}
	completer := filepath.Join(filepath.Dir(binary), "aws_completer")
	if _, err := os.Stat(completer); err != nil {
		completer, _ = exec.LookPath("aws_completer")
	}

	if completer != "" && strings.HasPrefix(line, "aws ") {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		env = append(env, "COMP_LINE="+line, "COMP_POINT="+strconv.Itoa(len(line)))
		cmd := shellProcess(ctx, completer, nil, env, dir)
		// Bound completer output with the same streaming cap, without sending it to the client.
		output := &strings.Builder{}
		cmd.Stdout = &shellCompletionWriter{w: output, remaining: shellMaxOutput, cancel: cancel}
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			candidates = strings.Fields(output.String())
		}

		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	_ = json.NewEncoder(w).Encode(shellCandidates{Candidates: candidates})
}

type shellCompletionWriter struct {
	w         io.Writer
	cancel    context.CancelFunc
	remaining int
}

func (w *shellCompletionWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.remaining {
		w.cancel()
		return 0, errors.New("completion output limit exceeded")
	}

	w.remaining -= n
	return w.w.Write(p)
}
