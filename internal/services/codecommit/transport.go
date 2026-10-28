package codecommit

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// protocolPattern admits the values git clients send in Git-Protocol, such as "version=2".
const (
	uploadPack  = "upload-pack"
	receivePack = "receive-pack"
)

var protocolPattern = regexp.MustCompile(`^[a-z0-9=:._-]{1,64}$`)

// ServeGit serves git's smart HTTP protocol for one repository: the reference advertisement
// (GET info/refs?service=...) and the upload-pack and receive-pack exchanges that clone, fetch and push. rest is
// what follows the repository name in the path. A repository that does not exist is a 404; nothing creates one.
//
// The protocol is served by git itself, run in stateless-rpc mode as git http-backend does, so a stock client
// gets shallow clones, protocol version 2, thin packs and connectivity checks.
func (s *Service) ServeGit(w http.ResponseWriter, req *http.Request, scope request.Scope, name, rest string) {
	if _, err := validateRepoName(&name); err != nil {
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	}

	r, err := s.lookup(req.Context(), scope, name)
	if err != nil {
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	}

	if err = r.ensure(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch {
	case rest == "info/refs" && req.Method == http.MethodGet:
		s.advertise(w, req, r)
	case (rest == "git-upload-pack" || rest == "git-receive-pack") && req.Method == http.MethodPost:
		s.exchange(w, req, r, strings.TrimPrefix(rest, "git-"))
	default:
		http.NotFound(w, req)
	}
}

// service returns the git command for a smart HTTP service name.
func service(name string) (string, bool) {
	command, ok := strings.CutPrefix(name, "git-")
	return command, ok && (command == uploadPack || command == receivePack)
}

// protocol returns the GIT_PROTOCOL setting for a request, which asks for protocol version 1 or 2.
func protocol(req *http.Request) []string {
	value := req.Header.Get("Git-Protocol")
	if !protocolPattern.MatchString(value) {
		return nil
	}

	return []string{"GIT_PROTOCOL=" + value}
}

func (s *Service) advertise(w http.ResponseWriter, req *http.Request, r *repository) {
	name := req.URL.Query().Get("service")
	command, ok := service(name)
	if !ok {
		http.Error(w, "Only the smart git protocol is supported: use git 1.6.6 or newer.", http.StatusForbidden)
		return
	}

	env := protocol(req)
	out, err := git(req.Context(), r.dir, nil, env, command, "--stateless-rpc", "--advertise-refs", r.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	noCache(w)
	w.Header().Set("Content-Type", "application/x-"+name+"-advertisement")
	// Protocol version 2 starts with the capabilities; earlier versions announce the service first.
	if command != uploadPack || !strings.Contains(strings.Join(env, ""), "version=2") {
		line := "# service=" + name + "\n"
		// #nosec G705 -- the service name is one of two fixed values and the response is not HTML.
		_, _ = fmt.Fprintf(w, "%04x%s0000", len(line)+4, line)
	}

	// #nosec G705 -- the response is git's own output, served as an application/x-git type, not HTML.
	_, _ = w.Write(out)
}

func noCache(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

// flusher sends each write to the client at once, so progress and pack data stream.
type flusher struct {
	http.ResponseWriter
	written bool
}

func (f *flusher) Write(p []byte) (int, error) {
	f.written = true
	n, err := f.ResponseWriter.Write(p)
	_ = http.NewResponseController(f.ResponseWriter).Flush()
	return n, err
}

func (s *Service) exchange(w http.ResponseWriter, req *http.Request, r *repository, command string) {
	if req.Header.Get("Content-Type") != "application/x-git-"+command+"-request" {
		http.Error(w, "Unsupported media type.", http.StatusUnsupportedMediaType)
		return
	}

	body := io.Reader(req.Body)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			http.Error(w, "The request body is not valid gzip.", http.StatusBadRequest)
			return
		}

		defer func() { _ = zr.Close() }()
		body = zr
	}

	ctx := req.Context()
	var before map[string]string
	if command == receivePack && s.caller != nil {
		var err error
		if before, err = r.refs(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	noCache(w)
	w.Header().Set("Content-Type", "application/x-git-"+command+"-result")
	out := &flusher{ResponseWriter: w}
	err := stream(ctx, r.dir, body, out, protocol(req), command, "--stateless-rpc", r.dir)
	if err != nil && !out.written {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if command == receivePack {
		s.pushed(context.WithoutCancel(ctx), r, before)
	}
}

// pushed settles a repository after a push: the first branch pushed becomes the default, and each created,
// updated or deleted reference is published to EventBridge.
func (s *Service) pushed(ctx context.Context, r *repository, before map[string]string) {
	// A failure here is repaired the next time the default branch is read.
	_, _, _ = r.defaultBranch(ctx)
	if s.caller == nil {
		return
	}

	after, err := r.refs(ctx)
	if err != nil {
		return
	}

	s.emitChanges(ctx, r, before, after)
}
