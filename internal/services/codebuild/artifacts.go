package codebuild

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) object(ctx context.Context, bucket, key, version string) ([]byte, error) {
	params := map[string]any{"Bucket": bucket, "Key": key}
	if version != "" {
		params["VersionId"] = version
	}

	out, err := s.caller.Base64Blobs().Call(ctx, "s3", "GetObject", params)
	if err != nil {
		return nil, err
	}

	body, ok := out["Body"].(string)
	if !ok {
		return nil, fmt.Errorf("S3 object body missing")
	}

	return base64.StdEncoding.DecodeString(body)
}

func (s *Service) download(ctx context.Context, r *buildRecord, dir string) (*buildspec, error) {
	if common.Value(r.Project.Source.Type) == s3Type {
		bucket, key, _ := strings.Cut(common.Value(r.Project.Source.Location), "/")
		data, err := s.object(ctx, bucket, key, common.Value(r.Build.SourceVersion))
		if err != nil {
			return nil, err
		}

		if err := extractSource(data, dir); err != nil {
			return nil, err
		}
	}

	value := common.Value(r.Project.Source.Buildspec)
	if strings.Contains(value, "\n") {
		return parseBuildspec([]byte(value))
	}

	if value == "" {
		value = "buildspec.yml"
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}

	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(value)
	if err != nil {
		return nil, err
	}

	return parseBuildspec(data)
}

func extractSource(data []byte, dir string) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("source must be a zip archive: %w", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	for _, entry := range archive.File {
		if !filepath.IsLocal(entry.Name) || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe source archive path")
		}

		if entry.FileInfo().IsDir() {
			if err := root.MkdirAll(entry.Name, 0o750); err != nil {
				return err
			}

			continue
		}

		if err := root.MkdirAll(filepath.Dir(entry.Name), 0o750); err != nil {
			return err
		}

		reader, e := entry.Open()
		if e != nil {
			return e
		}

		file, e := root.OpenFile(entry.Name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, entry.Mode().Perm()&0o777)
		if e != nil {
			_ = reader.Close()
			return e
		}

		// #nosec G110 -- local build archives have no additional CodeBuild size quota.
		_, e = io.Copy(file, reader)
		closeErr := file.Close()
		_ = reader.Close()
		if e != nil {
			return e
		}

		if closeErr != nil {
			return closeErr
		}
	}

	return nil
}

// matchArtifact treats ** as zero or more directories, matching CodeBuild's recursive file patterns.
func matchArtifact(pattern, name string) bool {
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func([]string, []string) bool
	match = func(p, n []string) bool {
		if len(p) == 0 {
			return len(n) == 0
		}

		if p[0] == "**" {
			return match(p[1:], n) || (len(n) > 0 && match(p, n[1:]))
		}

		if len(n) == 0 {
			return false
		}

		ok, _ := path.Match(p[0], n[0])
		return ok && match(p[1:], n[1:])
	}

	return match(p, n)
}

func (s *Service) upload(ctx context.Context, r *buildRecord, spec *buildspec, dir string) (string, error) {
	a := r.Project.Artifacts
	if common.Value(a.Type) == noArtifacts {
		return "", nil
	}

	if spec.Artifacts == nil {
		return "", fmt.Errorf("S3 artifacts require buildspec artifacts.files")
	}

	base := spec.Artifacts.BaseDirectory
	if base == "" {
		base = "."
	}

	// os.Root confines reads even if a build creates symlinks pointing outside its workspace.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}

	defer func() { _ = root.Close() }()
	files := map[string][]byte{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		relative := name
		if base != "." {
			// base-directory can include a single directory glob; its directory itself is discarded.
			found := false
			for i := range len(strings.Split(name, "/")) {
				parts := strings.Split(name, "/")
				if matchArtifact(base, strings.Join(parts[:i], "/")) {
					relative = strings.Join(parts[i:], "/")
					found = true
					break
				}
			}

			if !found {
				return nil
			}
		}

		selected := false
		for _, pattern := range spec.Artifacts.Files {
			if matchArtifact(pattern, relative) {
				selected = true
				break
			}
		}

		if !selected {
			return nil
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact symlinks are unsupported")
		}

		data, e := root.ReadFile(name)
		if e != nil {
			return e
		}

		if spec.Artifacts.DiscardPaths {
			relative = path.Base(relative)
		}

		if _, exists := files[relative]; exists {
			return fmt.Errorf("artifact discard-paths produced a duplicate name")
		}

		files[relative] = data
		return nil
	})
	if err != nil {
		return "", err
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no matching artifact files")
	}

	name := common.Value(a.Name)
	if common.Value(a.OverrideArtifactName) && spec.Artifacts.Name != "" {
		name = spec.Artifacts.Name
		if strings.ContainsAny(name, "$`\x00") {
			return "", fmt.Errorf("dynamic artifact names are not implemented")
		}
	}

	prefix := common.Value(a.Path)
	if common.Value(a.NamespaceType) == "BUILD_ID" {
		_, uuid, _ := strings.Cut(common.Value(r.Build.Id), ":")
		prefix = path.Join(prefix, uuid)
	}

	if name != "/" {
		prefix = path.Join(prefix, name)
	}

	put := func(key string, data []byte) error {
		_, e := s.caller.Base64Blobs().Call(ctx, "s3", "PutObject", map[string]any{
			"Bucket": common.Value(a.Location), "Key": key, "Body": base64.StdEncoding.EncodeToString(data),
		})
		return e
	}

	if common.Value(a.Packaging) == "ZIP" {
		var buf bytes.Buffer
		writer := zip.NewWriter(&buf)
		for name, data := range files {
			file, e := writer.Create(name)
			if e != nil {
				return "", e
			}

			if _, e = file.Write(data); e != nil {
				return "", e
			}
		}

		if err := writer.Close(); err != nil {
			return "", err
		}

		if err := put(prefix, buf.Bytes()); err != nil {
			return "", err
		}
	} else {
		for name, data := range files {
			if err := put(path.Join(prefix, name), data); err != nil {
				return "", err
			}
		}
	}

	return "arn:aws:s3:::" + common.Value(a.Location) + "/" + prefix, nil
}
