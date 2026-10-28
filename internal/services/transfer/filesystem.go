package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// The S3 in-process API takes []byte. Spool writes to disk, with a documented local
// 32 MiB per-file ceiling so final commit cannot allocate unbounded memory.
const maxUpload = 32 << 20

type filesystem struct {
	ctx      context.Context
	s        *Service
	user     *api.DescribedUser
	scope    request.Scope
	serverID string
}

func resolve(u *api.DescribedUser, name string) (bucket, key string, err error) {
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", "", syscall.EACCES
	}

	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	target := ""
	if common.Value(u.HomeDirectoryType) == directoryPath {
		home := common.Value(u.HomeDirectory)
		if clean != home && !strings.HasPrefix(clean, home+"/") {
			return "", "", syscall.EACCES
		}

		target = clean
	} else {
		// The longest matching entry wins, so "/logs" maps inside a chrooted "/".
		matched := -1
		for _, m := range u.HomeDirectoryMappings {
			entry := *m.Entry
			if len(entry) <= matched || clean != entry && entry != "/" && !strings.HasPrefix(clean, entry+"/") {
				continue
			}

			matched = len(entry)
			rest := strings.TrimPrefix(strings.TrimPrefix(clean, entry), "/")
			target = strings.TrimSuffix(*m.Target, "/") + "/" + rest
		}
	}

	if target == "" {
		return "", "", syscall.EACCES
	}

	parts := strings.SplitN(strings.TrimPrefix(target, "/"), "/", 2)
	if parts[0] == "" {
		return "", "", syscall.EACCES
	}

	bucket = parts[0]
	if len(parts) == 2 {
		key = strings.TrimSuffix(parts[1], "/")
	}

	return bucket, key, nil
}

func (h *filesystem) authorize(op, bucket, key string, extra map[string]any) error {
	params := map[string]any{"Bucket": bucket}
	if key != "" {
		params["Key"] = key
	}

	for k, v := range extra {
		params[k] = v
	}

	err := h.s.authorizer.Authorize(h.ctx, catalog.ServiceDelivery{
		Principal:     "transfer.amazonaws.com",
		SourceARN:     userARN(h.scope, h.serverID, *h.user.UserName),
		SourceAccount: h.scope.AccountID,
		Role: &catalog.DeliveryRole{
			ARN:       *h.user.Role,
			Session:   *h.user.UserName,
			Service:   "s3",
			Operation: op,
			Params:    params,
		},
	})
	if err != nil {
		return syscall.EACCES
	}

	return nil
}

func (h *filesystem) location(name string) (bucket, key string, err error) {
	bucket, key, err = resolve(h.user, name)
	if err != nil {
		return "", "", err
	}
	// HeadBucket's in-process implementation enforces the owning account and Region.
	_, err = h.s.buckets.HeadBucket(h.ctx, &s3api.HeadBucketRequest{Bucket: new(bucket)})
	if err != nil {
		return "", "", syscall.EACCES
	}

	return bucket, key, nil
}

type fileInfo struct {
	modified  time.Time
	name      string
	size      int64
	directory bool
}

func (f fileInfo) Name() string { return f.name }
func (f fileInfo) Size() int64  { return f.size }
func (f fileInfo) Mode() fs.FileMode {
	if f.directory {
		return fs.ModeDir | 0o755
	}

	return 0o644
}

func (f fileInfo) ModTime() time.Time { return f.modified }
func (f fileInfo) IsDir() bool        { return f.directory }
func (f fileInfo) Sys() any           { return nil }

type entries []fs.FileInfo

func (e entries) ListAt(dst []fs.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(e)) {
		return 0, io.EOF
	}

	n := copy(dst, e[offset:])
	if n < len(dst) {
		return n, io.EOF
	}

	return n, nil
}

func (h *filesystem) virtual(name string) entries {
	if common.Value(h.user.HomeDirectoryType) != "LOGICAL" {
		return nil
	}

	prefix := strings.TrimSuffix(path.Clean(name), "/") + "/"
	var out entries
	seen := map[string]bool{}
	for _, m := range h.user.HomeDirectoryMappings {
		if !strings.HasPrefix(*m.Entry, prefix) {
			continue
		}

		child := strings.SplitN(strings.TrimPrefix(*m.Entry, prefix), "/", 2)[0]
		if child != "" && !seen[child] {
			seen[child] = true
			out = append(out, fileInfo{name: child, directory: true})
		}
	}

	return out
}

func (h *filesystem) list(name string) (entries, error) {
	if virtual := h.virtual(name); len(virtual) > 0 {
		return virtual, nil
	}

	bucket, key, err := h.location(name)
	if err != nil {
		return nil, err
	}

	prefix := key
	if prefix != "" {
		prefix += "/"
	}

	if callErr := h.authorize(
		"ListObjectsV2",
		bucket,
		"",
		map[string]any{"Prefix": prefix, "Delimiter": "/"},
	); callErr != nil {
		return nil, callErr
	}

	var out entries
	var token *string
	for {
		page, e := h.s.buckets.ListObjectsV2(
			h.ctx,
			&s3api.ListObjectsV2Request{
				Bucket:            new(bucket),
				Prefix:            new(prefix),
				Delimiter:         new("/"),
				ContinuationToken: token,
			},
		)
		if e != nil {
			return nil, syscall.EACCES
		}

		for _, o := range page.Contents {
			if *o.Key == prefix {
				continue
			}

			out = append(
				out,
				fileInfo{name: path.Base(*o.Key), size: common.Value(o.Size), modified: common.Value(o.LastModified)},
			)
		}

		for _, p := range page.CommonPrefixes {
			out = append(out, fileInfo{name: path.Base(strings.TrimSuffix(*p.Prefix, "/")), directory: true})
		}

		token = page.NextContinuationToken
		if token == nil {
			break
		}
	}

	return out, nil
}

func (h *filesystem) stat(name string) (fs.FileInfo, error) {
	if len(h.virtual(name)) > 0 {
		return fileInfo{name: path.Base(name), directory: true}, nil
	}

	bucket, key, err := h.location(name)
	if err != nil {
		return nil, err
	}

	if key == "" {
		return fileInfo{name: path.Base(name), directory: true}, nil
	}

	if callErr := h.authorize("HeadObject", bucket, key, nil); callErr != nil {
		return nil, callErr
	}

	o, err := h.s.buckets.HeadObject(h.ctx, &s3api.HeadObjectRequest{Bucket: new(bucket), Key: new(key)})
	if err == nil {
		return fileInfo{
			name:     path.Base(key),
			size:     common.Value(o.ContentLength),
			modified: common.Value(o.LastModified),
		}, nil
	}

	children, err := h.list(name)
	if err != nil {
		return nil, err
	}

	if len(children) != 0 {
		return fileInfo{name: path.Base(key), directory: true}, nil
	}

	_, err = h.s.buckets.HeadObject(h.ctx, &s3api.HeadObjectRequest{Bucket: new(bucket), Key: new(key + "/")})
	if err == nil {
		return fileInfo{name: path.Base(key), directory: true}, nil
	}

	return nil, fs.ErrNotExist
}

func (h *filesystem) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		info, err := h.stat(r.Filepath)
		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			return nil, fs.ErrInvalid
		}

		return h.list(r.Filepath)
	case "Stat", "Lstat":
		info, err := h.stat(r.Filepath)
		if err != nil {
			return nil, err
		}

		return entries{info}, nil
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
}

type rangedReader struct {
	h           *filesystem
	bucket, key string
	size        int64
}

func (r *rangedReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}

	if off >= r.size {
		return 0, io.EOF
	}

	end := min(off+int64(len(p)), r.size) - 1
	o, err := r.h.s.buckets.GetObject(
		r.h.ctx,
		&s3api.GetObjectRequest{
			Bucket: new(r.bucket),
			Key:    new(r.key),
			Range:  new(fmt.Sprintf("bytes=%d-%d", off, end)),
		},
	)
	if err != nil {
		return 0, syscall.EACCES
	}

	n := copy(p, o.Body)
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (h *filesystem) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	info, err := h.stat(r.Filepath)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return nil, fs.ErrInvalid
	}

	bucket, key, err := h.location(r.Filepath)
	if err != nil {
		return nil, err
	}

	if callErr := h.authorize("GetObject", bucket, key, nil); callErr != nil {
		return nil, callErr
	}

	return &rangedReader{h: h, bucket: bucket, key: key, size: info.Size()}, nil
}

type upload struct {
	file   *os.File
	h      *filesystem
	bucket string
	key    string
	mu     sync.Mutex
	failed bool
}

func (u *upload) WriteAt(p []byte, off int64) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if off < 0 || off > maxUpload || int64(len(p)) > maxUpload-off {
		u.failed = true
		return 0, sftp.ErrSSHFxFailure
	}

	n, err := u.file.WriteAt(p, off)
	if err != nil {
		u.failed = true
	}

	return n, err
}

func (u *upload) TransferError(error) { u.mu.Lock(); u.failed = true; u.mu.Unlock() }
func (u *upload) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	defer func() { _ = os.Remove(u.file.Name()) }()
	defer func() { _ = u.file.Close() }()
	if u.failed {
		return sftp.ErrSSHFxFailure
	}

	if _, err := u.file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	body, err := io.ReadAll(io.LimitReader(u.file, maxUpload+1))
	if err != nil {
		return err
	}

	if len(body) > maxUpload {
		return sftp.ErrSSHFxFailure
	}

	_, err = u.h.s.buckets.PutObject(
		u.h.ctx,
		&s3api.PutObjectRequest{Bucket: new(u.bucket), Key: new(u.key), Body: body},
	)
	return err
}

func (h *filesystem) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	flags := r.Pflags()
	if flags.Append || !flags.Trunc {
		return nil, sftp.ErrSSHFxOpUnsupported
	}

	bucket, key, err := h.location(r.Filepath)
	if err != nil {
		return nil, err
	}

	if key == "" {
		return nil, syscall.EACCES
	}

	if callErr := h.authorize("PutObject", bucket, key, nil); callErr != nil {
		return nil, callErr
	}

	info, statErr := h.stat(r.Filepath)
	if statErr == nil && (info.IsDir() || flags.Excl) {
		return nil, fs.ErrExist
	}

	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, statErr
	}

	f, err := os.CreateTemp("", "tinystack-sftp-*")
	if err != nil {
		return nil, err
	}

	return &upload{file: f, h: h, bucket: bucket, key: key}, nil
}

func (h *filesystem) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Symlink", "Link", "Setstat":
		return sftp.ErrSSHFxOpUnsupported
	}

	bucket, key, err := h.location(r.Filepath)
	if err != nil {
		return err
	}

	if key == "" {
		return syscall.EACCES
	}

	info, statErr := h.stat(r.Filepath)
	switch r.Method {
	case "Mkdir":
		if statErr == nil {
			return fs.ErrExist
		}

		if !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}

		key += "/"
		if callErr := h.authorize("PutObject", bucket, key, nil); callErr != nil {
			return callErr
		}

		_, err = h.s.buckets.PutObject(
			h.ctx,
			&s3api.PutObjectRequest{Bucket: new(bucket), Key: new(key), Body: []byte{}},
		)
		return err
	case "Remove", "Rmdir":
		if statErr != nil {
			return statErr
		}

		if r.Method == "Rmdir" {
			if !info.IsDir() {
				return fs.ErrInvalid
			}

			children, e := h.list(r.Filepath)
			if e != nil {
				return e
			}

			if len(children) > 0 {
				return sftp.ErrSSHFxFailure
			}

			key += "/"
		} else if info.IsDir() {
			return syscall.EACCES
		}

		if callErr := h.authorize("DeleteObject", bucket, key, nil); callErr != nil {
			return callErr
		}

		_, err = h.s.buckets.DeleteObject(h.ctx, &s3api.DeleteObjectRequest{Bucket: new(bucket), Key: new(key)})
		return err
	case "Rename":
		if statErr != nil {
			return statErr
		}

		if info.IsDir() {
			return sftp.ErrSSHFxOpUnsupported
		}

		destBucket, destKey, e := h.location(r.Target)
		if e != nil {
			return e
		}

		_, e = h.stat(r.Target)
		if e == nil {
			return fs.ErrExist
		}

		if !errors.Is(e, fs.ErrNotExist) {
			return e
		}

		if callErr := h.authorize("GetObject", bucket, key, nil); callErr != nil {
			return callErr
		}

		source := url.PathEscape(bucket + "/" + key)
		if callErr := h.authorize(
			"CopyObject",
			destBucket,
			destKey,
			map[string]any{"CopySource": source},
		); callErr != nil {
			return callErr
		}

		if callErr := h.authorize("DeleteObject", bucket, key, nil); callErr != nil {
			return callErr
		}

		_, err = h.s.buckets.CopyObject(
			h.ctx,
			&s3api.CopyObjectRequest{Bucket: new(destBucket), Key: new(destKey), CopySource: new(source)},
		)
		if err != nil {
			return err
		}

		_, err = h.s.buckets.DeleteObject(h.ctx, &s3api.DeleteObjectRequest{Bucket: new(bucket), Key: new(key)})
		return err
	default:
		return sftp.ErrSSHFxOpUnsupported
	}
}

// OpenFile supports clients that request a read/write handle while creating a truncated file.
func (h *filesystem) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	writer, err := h.Filewrite(r)
	if err != nil {
		return nil, err
	}

	return writer.(*upload), nil
}

func (u *upload) ReadAt(p []byte, off int64) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.file.ReadAt(p, off)
}
