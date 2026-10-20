// Command sync vendors the Smithy models pinned in generator/model/manifest.json
// from aws-sdk-go-v2 at one commit, with the repository's license, and records
// that commit and each model's checksum in the manifest.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// modelDir is where aws-sdk-go-v2 keeps its Smithy models.
const modelDir = "codegen/sdk-codegen/aws-models"

// licenseFile is the upstream license vendored beside the models.
const licenseFile = "LICENSE.txt"

// maxFile bounds one download; the largest model (S3) is about 4 MB.
const maxFile = 64 << 20

// The GitHub endpoints; tests point them at a local server.
var (
	apiBase = "https://api.github.com/repos/"
	rawBase = "https://raw.githubusercontent.com/"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	filePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.json$`)
)

type model struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// manifest keeps its fields in the file's order rather than the most compact one.
//
//nolint:govet // fieldalignment would reorder the JSON.
type manifest struct {
	Repository string           `json:"repository"`
	Commit     string           `json:"commit"`
	License    string           `json:"license"`
	Models     map[string]model `json:"models"`
}

// source serves a GitHub repository's commits and files.
type source struct {
	client *http.Client
	api    string // https://api.github.com/repos/<owner>/<repo>
	raw    string // https://raw.githubusercontent.com/<owner>/<repo>
}

func github(client *http.Client, repository string) (source, error) {
	repo, ok := strings.CutPrefix(repository, "https://github.com/")
	if !ok || strings.Count(repo, "/") != 1 {
		return source{}, fmt.Errorf("repository %q is not a GitHub repository URL", repository)
	}

	return source{
		client: client,
		api:    apiBase + repo,
		raw:    rawBase + repo,
	}, nil
}

func (s source) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}

	if len(b) > maxFile {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxFile)
	}

	return b, nil
}

// resolve turns a branch, tag, or commit into a full commit hash.
func (s source) resolve(ctx context.Context, ref string) (string, error) {
	if commitPattern.MatchString(ref) {
		return ref, nil
	}

	b, err := s.get(ctx, s.api+"/commits/"+ref, "application/vnd.github.sha")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}

	commit := strings.TrimSpace(string(b))
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("resolve %s: unexpected commit %q", ref, commit)
	}

	return commit, nil
}

func (s source) file(ctx context.Context, commit, name string) ([]byte, error) {
	return s.get(ctx, s.raw+"/"+commit+"/"+name, "")
}

// addition pins a new model: name is the service's generator name and file the
// upstream model's file name.
type addition struct{ name, file string }

func parseAddition(arg string) (addition, error) {
	name, file, ok := strings.Cut(arg, "=")
	if !ok {
		file = name + ".json"
	}

	if !namePattern.MatchString(name) {
		return addition{}, fmt.Errorf("-add %q: the name must be lowercase letters and digits", arg)
	}

	if !filePattern.MatchString(file) {
		return addition{}, fmt.Errorf("-add %q: %q is not a model file name", arg, file)
	}

	return addition{name: name, file: file}, nil
}

// sync downloads every pinned model plus adds at ref and writes them into dir.
// Nothing is written unless every download succeeds.
func sync(ctx context.Context, client *http.Client, dir, ref string, adds []addition) ([]string, error) {
	// #nosec G304 -- the manifest is read from the selected local model directory.
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}

	var m manifest
	if jsonErr := json.Unmarshal(b, &m); jsonErr != nil {
		return nil, fmt.Errorf("manifest.json: %w", jsonErr)
	}

	if m.License == "" || filepath.Base(m.License) != m.License {
		return nil, fmt.Errorf("manifest.json: license %q is not a file name", m.License)
	}

	for _, add := range adds {
		if _, ok := m.Models[add.name]; ok {
			return nil, fmt.Errorf("-add %s: the model is already pinned", add.name)
		}

		if m.Models == nil {
			m.Models = map[string]model{}
		}

		m.Models[add.name] = model{Path: path.Join(modelDir, add.file)}
	}

	src, err := github(client, m.Repository)
	if err != nil {
		return nil, err
	}

	commit, err := src.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(m.Models))
	for name := range m.Models {
		names = append(names, name)
	}

	sort.Strings(names)
	files := map[string][]byte{}
	var report []string
	for _, name := range names {
		pinned := m.Models[name]
		data, fetchErr := cached(dir, name+".json", commit == m.Commit, pinned.SHA256)
		if data == nil && fetchErr == nil {
			data, fetchErr = src.file(ctx, commit, pinned.Path)
		}

		if fetchErr != nil {
			return nil, fmt.Errorf("%s: %w", name, fetchErr)
		}

		var smithy struct {
			Version string `json:"smithy"`
		}

		if jsonErr := json.Unmarshal(data, &smithy); jsonErr != nil || smithy.Version == "" {
			return nil, fmt.Errorf("%s: %s is not a Smithy JSON model", name, pinned.Path)
		}

		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		switch pinned.SHA256 {
		case "":
			report = append(report, name+": added")
		case sum:
			report = append(report, name+": unchanged")
		default:
			report = append(report, name+": updated")
		}

		files[name+".json"] = data
		m.Models[name] = model{Path: pinned.Path, SHA256: sum}
	}

	files[m.License], err = cached(dir, m.License, commit == m.Commit, "")
	if files[m.License] == nil && err == nil {
		files[m.License], err = src.file(ctx, commit, licenseFile)
	}

	if err != nil {
		return nil, fmt.Errorf("license: %w", err)
	}

	m.Commit = commit
	out, err := marshalManifest(b, m, adds)
	if err != nil {
		return nil, err
	}

	// The manifest goes last: an interrupted sync leaves files the old
	// checksums reject, which the generator reports instead of using them.
	for name, data := range files {
		if writeErr := os.WriteFile(filepath.Join(dir, name), data, 0o644); writeErr != nil {
			return nil, writeErr
		}
	}

	// #nosec G703 -- dir is the explicitly selected local model output directory.
	if writeErr := os.WriteFile(filepath.Join(dir, "manifest.json"), append(out, '\n'), 0o644); writeErr != nil {
		return nil, writeErr
	}

	return append(report, "commit: "+commit), nil
}

// marshalManifest preserves existing entry order when adding models at the same pin.
// A pin update still rewrites the complete manifest with its new checksums.
func marshalManifest(original []byte, updated manifest, adds []addition) ([]byte, error) {
	var previous manifest
	if err := json.Unmarshal(original, &previous); err != nil {
		return nil, err
	}

	if previous.Commit != updated.Commit {
		return json.MarshalIndent(updated, "", "  ")
	}

	for name, model := range previous.Models {
		if updated.Models[name] != model {
			return json.MarshalIndent(updated, "", "  ")
		}
	}

	if len(adds) == 0 {
		return bytes.TrimSpace(original), nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(original, &fields); err != nil {
		return nil, err
	}

	models := fields["models"]
	end := bytes.LastIndexByte(models, '}')
	if end < 0 {
		return nil, fmt.Errorf("manifest models must be an object")
	}

	var entries bytes.Buffer
	entries.Write(bytes.TrimSpace(models[:end]))
	hasEntries := len(previous.Models) > 0
	for _, add := range adds {
		if hasEntries {
			entries.WriteByte(',')
		}

		name, err := json.Marshal(add.name)
		if err != nil {
			return nil, err
		}

		value, err := json.Marshal(updated.Models[add.name])
		if err != nil {
			return nil, err
		}

		entries.Write(name)
		entries.WriteByte(':')
		entries.Write(value)
		hasEntries = true
	}

	entries.WriteByte('}')
	replaced := bytes.Replace(original, models, entries.Bytes(), 1)
	var out bytes.Buffer
	if err := json.Indent(&out, replaced, "", "  "); err != nil {
		return nil, err
	}

	return bytes.TrimSpace(out.Bytes()), nil
}

// cached returns the local copy of a pinned file when the sync stays on the
// pinned commit and the copy still matches its checksum (any copy when sum is
// empty), so a sync that only adds models downloads just those. It returns
// nil when the file must be downloaded.
func cached(dir, name string, samePin bool, sum string) ([]byte, error) {
	if !samePin {
		return nil, nil
	}

	// #nosec G304 -- name is a pinned model or license file in the local model directory.
	data, err := os.ReadFile(filepath.Join(dir, name))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case sum != "" && fmt.Sprintf("%x", sha256.Sum256(data)) != sum:
		return nil, nil
	}

	return data, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", ".", "repository root")
	ref := flag.String("ref", "main", "aws-sdk-go-v2 branch, tag, or commit to sync to")
	var adds []addition
	usage := "pin a new model as `name[=file.json]` (repeatable; file defaults to name.json)"
	flag.Func("add", usage, func(arg string) error {
		add, err := parseAddition(arg)
		if err != nil {
			return err
		}

		adds = append(adds, add)
		return nil
	})
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	report, err := sync(ctx, &http.Client{}, filepath.Join(*root, "generator", "model"), *ref, adds)
	if err != nil {
		return err
	}

	fmt.Println(strings.Join(report, "\n"))
	return nil
}
