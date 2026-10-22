package gateway

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

// maxPostFields caps the form fields before the file, as AWS does.
const maxPostFields = 20 << 10

// postForm is a browser upload: its fields, named in lower case as S3 matches
// them, and the file part, which ends the form.
type postForm struct {
	fields   map[string]string
	filename string
	fileType string
	file     []byte
}

// postPolicy is the decoded policy document. Each condition is an object with
// one field and its exact value, or an [operator, field, value] array.
type postPolicy struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

type postResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string
	Bucket   string
	Key      string
	ETag     string
}

// postIgnored are the fields a policy need not cover.
func postIgnored(name string) bool {
	return name == "policy" || name == "x-amz-signature" || name == "file" || strings.HasPrefix(name, "x-ignore-")
}

// postSupported reports whether TinyStack applies the field to the upload.
func postSupported(name string) bool {
	switch name {
	case "key", "content-type", "success_action_status", "success_action_redirect", "redirect",
		"x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-security-token",
		"x-amz-object-lock-mode", "x-amz-object-lock-retain-until-date", "x-amz-object-lock-legal-hold":
		return true
	}

	return postIgnored(name) || strings.HasPrefix(name, "x-amz-meta-")
}

func postDenied(format string, args ...any) error {
	return &awserror.Error{
		Code: accessDenied, Message: "Invalid according to Policy: " + fmt.Sprintf(format, args...),
		StatusCode: http.StatusForbidden,
	}
}

func postMalformed(message string) error {
	return &awserror.Error{Code: "MalformedPOSTRequest", Message: message, StatusCode: http.StatusBadRequest}
}

func invalidPolicy(message string) error {
	return &awserror.Error{
		Code: "InvalidPolicyDocument", Message: "Invalid Policy: " + message, StatusCode: http.StatusBadRequest,
	}
}

// readPostForm reads the fields up to the file part; S3 ignores anything after it.
func readPostForm(r *http.Request, maxFile int64) (*postForm, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, postMalformed("The body of your POST request is not well-formed multipart/form-data.")
	}

	form := &postForm{fields: map[string]string{}}
	size := 0
	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			return nil, &awserror.Error{
				Code: "InvalidArgument", Message: "POST requires exactly one file upload per request.",
				StatusCode: http.StatusBadRequest,
			}
		}

		if nextErr != nil {
			return nil, postMalformed(nextErr.Error())
		}

		name := strings.ToLower(part.FormName())
		if name == "file" {
			var buf bytes.Buffer
			n, copyErr := io.Copy(&buf, io.LimitReader(part, maxFile+1))
			if copyErr != nil {
				return nil, postMalformed(copyErr.Error())
			}

			if n > maxFile {
				return nil, entityTooLarge()
			}

			form.file, form.filename, form.fileType = buf.Bytes(), part.FileName(), part.Header.Get("Content-Type")
			return form, nil
		}

		value, readErr := io.ReadAll(io.LimitReader(part, int64(maxPostFields-size+1)))
		if readErr != nil {
			return nil, postMalformed(readErr.Error())
		}

		size += len(value)
		if size > maxPostFields {
			return nil, postMalformed("Your POST request fields preceding the upload file were too large.")
		}

		if _, dup := form.fields[name]; dup {
			return nil, postMalformed("The form field " + part.FormName() + " is repeated.")
		}

		form.fields[name] = string(value)
	}
}

func entityTooLarge() error {
	return &awserror.Error{
		Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed size",
		StatusCode: http.StatusBadRequest,
	}
}

// checkPostPolicy enforces the policy's expiration and conditions, and that
// every field other than the ignored ones is covered by a condition.
func checkPostPolicy(encoded string, form *postForm, bucket string, now time.Time) error {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return invalidPolicy("Policy could not be base64 decoded.")
	}

	var policy postPolicy
	if err = json.Unmarshal(raw, &policy); err != nil {
		return invalidPolicy("Policy is not valid JSON.")
	}

	expires, err := time.Parse(time.RFC3339, policy.Expiration)
	if err != nil {
		return invalidPolicy("Invalid 'expiration' value: " + strconv.Quote(policy.Expiration))
	}

	if !now.Before(expires) {
		return postDenied("Policy expired.")
	}

	value := func(field string) string {
		if field == "bucket" {
			return bucket
		}

		return form.fields[field]
	}
	covered := map[string]bool{}
	for _, condition := range policy.Conditions {
		var exact map[string]string
		if json.Unmarshal(condition, &exact) == nil {
			for field, want := range exact {
				field = strings.ToLower(field)
				covered[field] = true
				if value(field) != want {
					return postDenied("Policy Condition failed: [\"eq\", \"$%s\", %q]", field, want)
				}
			}

			continue
		}

		var rule []json.RawMessage
		var operator string
		if json.Unmarshal(condition, &rule) != nil || len(rule) != 3 || json.Unmarshal(rule[0], &operator) != nil {
			return invalidPolicy("Invalid Condition: " + string(condition))
		}

		operator = strings.ToLower(operator)
		if operator == "content-length-range" {
			var low, high int64
			if json.Unmarshal(rule[1], &low) != nil || json.Unmarshal(rule[2], &high) != nil {
				return invalidPolicy("Invalid content-length-range: " + string(condition))
			}

			if size := int64(len(form.file)); size < low {
				return &awserror.Error{
					Code: "EntityTooSmall", Message: "Your proposed upload is smaller than the minimum allowed size",
					StatusCode: http.StatusBadRequest,
				}
			} else if size > high {
				return entityTooLarge()
			}

			continue
		}

		var field, want string
		if json.Unmarshal(rule[1], &field) != nil || json.Unmarshal(rule[2], &want) != nil ||
			!strings.HasPrefix(field, "$") {
			return invalidPolicy("Invalid Condition: " + string(condition))
		}

		field = strings.ToLower(field[1:])
		covered[field] = true
		switch operator {
		case "eq":
			if value(field) != want {
				return postDenied("Policy Condition failed: [\"eq\", \"$%s\", %q]", field, want)
			}
		case "starts-with":
			if !strings.HasPrefix(value(field), want) {
				return postDenied("Policy Condition failed: [\"starts-with\", \"$%s\", %q]", field, want)
			}
		default:
			return invalidPolicy("Invalid Condition operator: " + operator)
		}
	}

	for field := range form.fields {
		if !postIgnored(field) && !covered[field] {
			return postDenied("Extra input fields: %s", field)
		}
	}

	return nil
}

// postCredentials applies the form's x-amz-credential to the call as identify
// does for a signed request, and in verify mode checks the policy signature.
func (s *Server) postCredentials(r *http.Request, c *call, form *postForm) error {
	credential := form.fields["x-amz-credential"]
	verify := s.cfg.Identity.Mode == config.IdentityVerify
	if credential == "" {
		if verify {
			return authError(catalog.RESTXML, missingToken)
		}

		return nil
	}

	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[0] == "" || scope[3] != "s3" || scope[4] != "aws4_request" {
		return awserror.New("InvalidArgument", "Invalid x-amz-credential: "+credential)
	}

	c.meta.AccessKey, c.meta.Region = scope[0], scope[2]
	if account, _, ok := s.keyAccount(scope[0]); ok {
		c.meta.AccountID = account
	}

	if err := s.resolveCredentials(r.Context(), c); err != nil {
		return err
	}

	if !verify {
		return nil
	}

	if form.fields["x-amz-algorithm"] != sigv4.Algorithm || form.fields["policy"] == "" ||
		!strings.HasPrefix(form.fields["x-amz-date"], scope[1]) {
		return awserror.New("InvalidArgument", "POST requires a policy signed with AWS4-HMAC-SHA256 for its credential")
	}

	secret, ok := s.cfg.Identity.SecretKeys[scope[0]]
	if !ok {
		if c.credentials == nil ||
			subtle.ConstantTimeCompare([]byte(form.fields["x-amz-security-token"]), []byte(c.credentials.Token)) != 1 {
			return authError(catalog.RESTXML, unknownKey)
		}

		secret = c.credentials.Secret
	}

	key := sigv4.SigningKey(secret, scope[1], scope[2], scope[3])
	want := hex.EncodeToString(sigv4.HMACSHA256(key, form.fields["policy"]))
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(form.fields["x-amz-signature"]))) != 1 {
		return authError(catalog.RESTXML, badSignature)
	}

	return nil
}

// postObject builds the PutObject request the form describes.
func postObject(form *postForm, bucket, key string) (*api.PutObjectRequest, error) {
	for field := range form.fields {
		if !postSupported(field) {
			return nil, rest.NotImplemented("TinyStack does not support the POST form field " + field + ".")
		}
	}

	in := &api.PutObjectRequest{Bucket: new(bucket), Key: new(key), Body: form.file}
	for field, value := range form.fields {
		if name, ok := strings.CutPrefix(field, "x-amz-meta-"); ok {
			if in.Metadata == nil {
				in.Metadata = map[string]string{}
			}

			in.Metadata[name] = value
		}
	}

	if contentType := form.fields["content-type"]; contentType != "" {
		in.ContentType = new(contentType)
	} else if form.fileType != "" {
		in.ContentType = new(form.fileType)
	}

	if mode := form.fields["x-amz-object-lock-mode"]; mode != "" {
		in.ObjectLockMode = new(mode)
	}

	if hold := form.fields["x-amz-object-lock-legal-hold"]; hold != "" {
		in.ObjectLockLegalHoldStatus = new(hold)
	}

	if until := form.fields["x-amz-object-lock-retain-until-date"]; until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return nil, awserror.New("InvalidArgument", "x-amz-object-lock-retain-until-date must be an ISO 8601 date")
		}

		in.ObjectLockRetainUntilDate = &t
	}

	return in, nil
}

// serveS3PostUpload stores a browser form upload (multipart/form-data POST to
// a bucket) through PutObject, with its authorization, faults, and behavior.
func (s *Server) serveS3PostUpload(w http.ResponseWriter, r *http.Request, c *call) {
	bucket := c.labels["Bucket"]
	maxFile := s.cfg.Storage.MaxObjectBytes
	if maxFile <= 0 {
		maxFile = 5 << 30
	}

	form, err := readPostForm(r, maxFile)
	if err == nil {
		err = s.postCredentials(r, c, form)
	}

	if err == nil && form.fields["policy"] != "" {
		err = checkPostPolicy(form.fields["policy"], form, bucket, time.Now())
	}

	var in *api.PutObjectRequest
	key := ""
	if err == nil {
		if key = strings.ReplaceAll(form.fields["key"], "${filename}", form.filename); key == "" {
			err = awserror.New("InvalidArgument", "Bucket POST must contain a field named 'key'.")
		}
	}

	if err == nil {
		in, err = postObject(form, bucket, key)
	}

	op, ok := c.service.Operations["PutObject"]
	if err == nil && !ok {
		err = rest.NotImplemented("PutObject is not implemented")
	}

	ctx := request.WithMetadata(r.Context(), c.meta)
	if err == nil {
		err = s.authorize(ctx, r, c, &op, in)
	}

	if err == nil {
		err = s.inject(r.Context(), c)
	}

	var output any
	if err == nil {
		output, err = op.Invoke(ctx, in)
	}

	if err != nil {
		s.fail(w, c, err)
		return
	}

	out, _ := output.(*api.PutObjectOutput)
	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
		w.Header().Set("ETag", etag)
	}

	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}

	redirect := form.fields["success_action_redirect"]
	if redirect == "" {
		redirect = form.fields["redirect"]
	}

	if u, parseErr := url.Parse(redirect); redirect != "" && parseErr == nil {
		q := u.Query()
		q.Set("bucket", bucket)
		q.Set("key", key)
		q.Set("etag", etag)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}

	switch form.fields["success_action_status"] {
	case "201":
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}

		location := fmt.Sprintf("%s://%s/%s/%s", scheme, r.Host, bucket, url.PathEscape(key))
		if virtualBucket(r.Host) != "" {
			location = fmt.Sprintf("%s://%s/%s", scheme, r.Host, url.PathEscape(key))
		}

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusCreated)
		_ = xml.NewEncoder(w).Encode(postResponse{Location: location, Bucket: bucket, Key: key, ETag: etag})
	case "200":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
