package codecommit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// zeroID is the all-zero object name git uses for "no object" in ref updates.
const zeroID = "0000000000000000000000000000000000000000"

// gitError is a git command that exited unsuccessfully.
type gitError struct {
	stderr string
	args   []string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), e.stderr)
}

// errGit is what API callers see when the git executable cannot be started.
func errGit(err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return &awserror.Error{
			Code:       "ServiceUnavailableException",
			Message:    "git was not found on PATH: CodeCommit repository content requires the git executable",
			StatusCode: 503,
		}
	}

	return err
}

// gitEnv isolates git from the user's configuration and makes paths literal, so a path
// named like a pathspec magic word or glob is just a path.
func gitEnv(dir string, extra ...string) []string {
	env := append(os.Environ(),
		"GIT_DIR="+dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)

	return append(env, extra...)
}

// git runs one git command against the bare repository in dir and returns its standard output.
func git(ctx context.Context, dir string, stdin []byte, extraEnv []string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return nil, errGit(err)
	}

	// #nosec G204 G702 -- arguments are built by this package and run without a shell.
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = gitEnv(dir, extraEnv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		return nil, &gitError{args: args, stderr: strings.TrimSpace(stderr.String())}
	}

	return stdout.Bytes(), nil
}

// stream runs a git command whose standard input and output are an HTTP exchange or an archive.
func stream(
	ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, extraEnv []string, args ...string,
) error {
	path, err := exec.LookPath("git")
	if err != nil {
		return errGit(err)
	}

	// #nosec G204 G702 -- arguments are built by this package and run without a shell.
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = gitEnv(dir, extraEnv...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return &gitError{args: args, stderr: strings.TrimSpace(stderr.String())}
	}

	return nil
}
