package ssm

import (
	"context"
	"crypto/sha1" // #nosec G505 -- Sha1 is a document field AWS returns, not a security measure.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	ownerAmazon     = "Amazon"
	documentActive  = "Active"
	hashSha256      = "Sha256"
	typeCommandDoc  = "Command"
	maxDocVersions  = 1000
	maxShared       = 20
	maxDocListing   = 50
	maxVersionsPage = 50
	sharedWithAll   = "all"
	defaultShare    = "$DEFAULT"
	latestSelector  = "$LATEST"
	permissionShare = "Share"
	documentKind    = "Document"
)

// documentTypes are the document types CreateDocument accepts.
var documentTypes = []string{
	typeCommandDoc, "Policy", "Automation", "Session", "Package", "ApplicationConfiguration",
	"ApplicationConfigurationSchema", "DeploymentStrategy", "ChangeCalendar", "Automation.ChangeTemplate",
	"ProblemAnalysis", "ProblemAnalysisTemplate", "CloudFormation", "ConformancePackTemplate", "QuickSetup",
}

var documentName = regexp.MustCompile(`^[a-zA-Z0-9_\-.]{3,128}$`)

// docRecord is a document's mutable record; each version is a docVersion.
type docRecord struct {
	Tags map[string]string `json:",omitempty"`
	// Shared maps the accounts the document is shared with to the version
	// they get. Sharing is recorded and reported, not enforced.
	Shared  map[string]string `json:",omitempty"`
	Name    string
	Type    string
	Owner   string
	Default int64
	Latest  int64
}

// docVersion is one immutable document version.
type docVersion struct {
	Content     string
	Format      string
	DisplayName string `json:",omitempty"`
	VersionName string `json:",omitempty"`
	TargetType  string `json:",omitempty"`
	Version     int64
	CreatedAt   int64 // Unix milliseconds
}

func docKey(scope request.Scope, name string) string { return scope.Key("ssm", "d", name) }

// docVersionPrefix orders a document's versions: names cannot contain ":".
func docVersionPrefix(scope request.Scope, name string) string {
	return scope.Key("ssm", "dv", name) + ":"
}

func docVersionKey(scope request.Scope, name string, v int64) string {
	return docVersionPrefix(scope, name) + fmt.Sprintf("%010d", v)
}

// documentARN returns a document's ARN; AWS-owned documents have no account.
func documentARN(scope request.Scope, rec *docRecord) string {
	account := scope.AccountID
	if rec.Owner == ownerAmazon {
		account = ""
	}

	return "arn:aws:ssm:" + scope.Region + ":" + account + ":document/" + rec.Name
}

func invalidDocument(format string, args ...any) error {
	return awserror.New("InvalidDocument", fmt.Sprintf(format, args...))
}

func invalidDocumentVersion(selector string) error {
	return awserror.New("InvalidDocumentVersion", "The document version is not valid or does not exist: "+selector)
}

func invalidOperation(message string) error {
	return awserror.New("InvalidDocumentOperation", message)
}

// builtinCreated is when the AWS-owned documents were created, in Unix
// milliseconds.
const builtinCreated = 1577836800000

const runShellScript = `{"schemaVersion":"2.2","description":"Run a shell script or specify the commands to run.",` +
	`"parameters":{"commands":{"type":"StringList","description":"(Required) Specify a shell script or a command ` +
	`to run.","displayType":"textarea"},"workingDirectory":{"type":"String","default":"","description":"(Optional) ` +
	`The path to the working directory on your instance.","maxChars":4096},"executionTimeout":{"type":"String",` +
	`"default":"3600","description":"(Optional) The time in seconds for a command to complete before it is ` +
	`considered to have failed. Default is 3600 (1 hour). Maximum is 172800 (48 hours).","allowedPattern":` +
	`"([1-9][0-9]{0,4})|(1[0-6][0-9]{4})|(17[0-1][0-9]{3})|(172[0-7][0-9]{2})|(172800)"}},"mainSteps":[{"action":` +
	`"aws:runShellScript","name":"runShellScript","inputs":{"timeoutSeconds":"{{ executionTimeout }}","runCommand":` +
	`"{{ commands }}","workingDirectory":"{{ workingDirectory }}"}}]}`

const runPowerShellScript = `{"schemaVersion":"2.2","description":"Run a PowerShell script or specify the ` +
	`paths to scripts to run.","parameters":{"commands":{"type":"StringList","description":"(Required) Specify a ` +
	`PowerShell script or a command to run.","displayType":"textarea"},"workingDirectory":{"type":"String",` +
	`"default":"","description":"(Optional) The path to the working directory on your instance.","maxChars":4096},` +
	`"executionTimeout":{"type":"String","default":"3600","description":"(Optional) The time in seconds for a ` +
	`command to complete before it is considered to have failed. Default is 3600 (1 hour). Maximum is 172800 ` +
	`(48 hours).","allowedPattern":"([1-9][0-9]{0,4})|(1[0-6][0-9]{4})|(17[0-1][0-9]{3})|(172[0-7][0-9]{2})|` +
	`(172800)"}},"mainSteps":[{"action":"aws:runPowerShellScript","name":"runPowerShellScript","inputs":` +
	`{"timeoutSeconds":"{{ executionTimeout }}","runCommand":"{{ commands }}","workingDirectory":` +
	`"{{ workingDirectory }}"}}]}`

// builtins are the AWS-owned documents, at version 1, by name. They belong to
// no account and cannot be changed, tagged, or deleted.
var builtins = map[string]string{
	"AWS-RunShellScript":      runShellScript,
	"AWS-RunPowerShellScript": runPowerShellScript,
}

func builtin(name string) (*docRecord, *docVersion, bool) {
	text, ok := builtins[name]
	if !ok {
		return nil, nil, false
	}

	return &docRecord{Name: name, Type: typeCommandDoc, Owner: ownerAmazon, Default: 1, Latest: 1},
		&docVersion{Content: text, Format: formatJSON, Version: 1, CreatedAt: builtinCreated}, true
}

// documentRef reduces a document name or ARN to the name.
func documentRef(ref *string) (string, error) {
	name := common.Value(ref)
	if rest, ok := strings.CutPrefix(name, "arn:"); ok {
		_, after, found := strings.Cut(rest, ":document/")
		if !found {
			return "", invalidDocument("The document ARN is not valid: %s", name)
		}

		name = after
	}

	if !documentName.MatchString(name) {
		return "", invalidDocument("The document name is not valid: %s", name)
	}

	return name, nil
}

// loadDocument returns the AWS-owned or stored document called name.
func loadDocument(tx storage.ReadTx, scope request.Scope, name string) (*docRecord, error) {
	if rec, _, ok := builtin(name); ok {
		return rec, nil
	}

	rec, found, err := storage.Read[docRecord](tx, docKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, invalidDocument("The specified document does not exist.")
	}

	return rec, nil
}

// loadDocVersions returns every stored version of a document, oldest first.
func loadDocVersions(tx storage.ReadTx, scope request.Scope, rec *docRecord) ([]*docVersion, error) {
	if _, v, ok := builtin(rec.Name); ok {
		return []*docVersion{v}, nil
	}

	var out []*docVersion
	for _, k := range tx.Keys(docVersionPrefix(scope, rec.Name)) {
		v, _, err := storage.Read[docVersion](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}

// pickVersion selects the version a DocumentVersion and VersionName
// request pair names: the default when both are absent.
func pickVersion(rec *docRecord, versions []*docVersion, number, name *string) (*docVersion, error) {
	selector := common.Value(number)
	var want int64
	switch selector {
	case "", defaultShare:
		want = rec.Default
	case latestSelector:
		want = rec.Latest
	default:
		n, err := strconv.ParseInt(selector, 10, 64)
		if err != nil || n < 1 {
			return nil, invalidDocumentVersion(selector)
		}

		want = n
	}

	if name != nil && number == nil {
		i := slices.IndexFunc(versions, func(v *docVersion) bool { return v.VersionName == *name })
		if i < 0 {
			return nil, invalidDocumentVersion(*name)
		}

		return versions[i], nil
	}

	i := slices.IndexFunc(versions, func(v *docVersion) bool { return v.Version == want })
	if i < 0 || (name != nil && versions[i].VersionName != *name) {
		return nil, invalidDocumentVersion(selector)
	}

	return versions[i], nil
}

// find loads a document and the version a request selects.
func (s *Service) find(tx storage.ReadTx, scope request.Scope, ref, number, name *string) (
	*docRecord,
	*docVersion,
	error,
) {
	docName, err := documentRef(ref)
	if err != nil {
		return nil, nil, err
	}

	rec, err := loadDocument(tx, scope, docName)
	if err != nil {
		return nil, nil, err
	}

	versions, err := loadDocVersions(tx, scope, rec)
	if err != nil {
		return nil, nil, err
	}

	v, err := pickVersion(rec, versions, number, name)

	return rec, v, err
}

func sha(text string) (sha1Hex, sha256Hex string) {
	one := sha1.Sum([]byte(text)) // #nosec G401 -- see the import.
	two := sha256.Sum256([]byte(text))

	return hex.EncodeToString(one[:]), hex.EncodeToString(two[:])
}

// describeDocument describes version v of rec.
func describeDocument(rec *docRecord, v *docVersion) (*api.DocumentDescription, error) {
	c, err := parseContent(v.Content, v.Format, rec.Type)
	if err != nil {
		return nil, err
	}

	one, two := sha(v.Content)
	out := &api.DocumentDescription{
		Sha1: new(one), Hash: new(two), HashType: new(hashSha256), Name: new(rec.Name),
		DisplayName: optional(v.DisplayName), VersionName: optional(v.VersionName), Owner: new(rec.Owner),
		CreatedDate: seconds(v.CreatedAt), Status: new(documentActive),
		DocumentVersion: new(strconv.FormatInt(v.Version, 10)),
		Description:     optional(c.Description), Parameters: c.Parameters, DocumentType: new(rec.Type),
		SchemaVersion: optional(c.Schema), LatestVersion: new(strconv.FormatInt(rec.Latest, 10)),
		DefaultVersion: new(strconv.FormatInt(rec.Default, 10)), DocumentFormat: new(v.Format),
		TargetType: optional(v.TargetType), Tags: tagList(rec.Tags),
	}
	if rec.Type == typeCommandDoc {
		out.PlatformTypes = c.platforms()
	}

	if rec.Owner == ownerAmazon {
		out.Author = new(ownerAmazon)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	var out []api.Tag
	for _, k := range sortedKeys(tags) {
		out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// checkFormat returns the format of a request, JSON by default.
func checkFormat(format *string) (string, error) {
	switch got := value(format, formatJSON); got {
	case formatJSON, formatYAML, formatText:
		return got, nil
	default:
		return "", validation("DocumentFormat must be JSON, YAML, or TEXT.")
	}
}

func checkVersionName(name *string) error {
	if name != nil && (len(*name) > 128 || !regexp.MustCompile(`^[a-zA-Z0-9_\-.]{1,128}$`).MatchString(*name)) {
		return validation("VersionName must be 1 to 128 letters, digits, and the symbols _-.")
	}

	return nil
}

// newVersion builds a document version from a Create or Update request.
func newVersion(text, format, displayName, versionName, targetType *string, number, at int64) (
	*docVersion,
	error,
) {
	if text == nil || *text == "" {
		return nil, validation("Content is required.")
	}

	f, err := checkFormat(format)
	if err != nil {
		return nil, err
	}

	if nameErr := checkVersionName(versionName); nameErr != nil {
		return nil, nameErr
	}

	return &docVersion{
		Content: *text, Format: f, DisplayName: value(displayName, ""), VersionName: value(versionName, ""),
		TargetType: value(targetType, ""), Version: number, CreatedAt: at,
	}, nil
}

func (s *Service) CreateDocument(
	ctx context.Context,
	in *api.CreateDocumentRequest,
) (*api.CreateDocumentResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.Name, "")
	if !documentName.MatchString(name) {
		return nil, validation("Document name must be 3 to 128 letters, digits, and the symbols _-.")
	}

	if lower := strings.ToLower(name); strings.HasPrefix(lower, "aws-") || strings.HasPrefix(lower, "amazon") ||
		strings.HasPrefix(lower, "amzn") {
		return nil, invalidDocument("Document names can't begin with aws-, amazon, or amzn.")
	}

	docType := value(in.DocumentType, typeCommandDoc)
	if !slices.Contains(documentTypes, docType) {
		return nil, validation("DocumentType is not supported: %s", docType)
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	v, err := newVersion(in.Content, in.DocumentFormat, in.DisplayName, in.VersionName, in.TargetType, 1,
		s.now().UnixMilli())
	if err != nil {
		return nil, err
	}

	if _, parseErr := parseContent(v.Content, v.Format, docType); parseErr != nil {
		return nil, parseErr
	}

	rec := &docRecord{
		Name: name, Type: docType, Owner: m.AccountID, Default: 1, Latest: 1, Tags: tags,
	}
	var out *api.DocumentDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(docKey(m.Scope, name)); exists {
			return awserror.New("DocumentAlreadyExists", "The specified document already exists.")
		}

		if writeErr := storage.Write(tx, docVersionKey(m.Scope, name, 1), v); writeErr != nil {
			return writeErr
		}

		if writeErr := storage.Write(tx, docKey(m.Scope, name), rec); writeErr != nil {
			return writeErr
		}

		var descErr error
		out, descErr = describeDocument(rec, v)

		return descErr
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDocumentResult{DocumentDescription: out}, nil
}

func (s *Service) GetDocument(ctx context.Context, in *api.GetDocumentRequest) (*api.GetDocumentResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.GetDocumentResult
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, v, findErr := s.find(tx, m.Scope, in.Name, in.DocumentVersion, in.VersionName)
		if findErr != nil {
			return findErr
		}

		format := value(in.DocumentFormat, v.Format)
		text, convertErr := convert(v.Content, v.Format, format)
		if convertErr != nil {
			return convertErr
		}

		out = &api.GetDocumentResult{
			Name: new(rec.Name), CreatedDate: seconds(v.CreatedAt), DisplayName: optional(v.DisplayName),
			VersionName: optional(v.VersionName), DocumentVersion: new(strconv.FormatInt(v.Version, 10)),
			Status: new(documentActive), Content: new(text), DocumentType: new(rec.Type), DocumentFormat: new(format),
		}

		return nil
	})

	return out, err
}

func (s *Service) DescribeDocument(
	ctx context.Context,
	in *api.DescribeDocumentRequest,
) (*api.DescribeDocumentResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeDocumentResult{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, v, findErr := s.find(tx, m.Scope, in.Name, in.DocumentVersion, in.VersionName)
		if findErr != nil {
			return findErr
		}

		var descErr error
		out.Document, descErr = describeDocument(rec, v)

		return descErr
	})

	return out, err
}

func (s *Service) UpdateDocument(
	ctx context.Context,
	in *api.UpdateDocumentRequest,
) (*api.UpdateDocumentResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := documentRef(in.Name)
	if err != nil {
		return nil, err
	}

	if _, _, isBuiltin := builtin(name); isBuiltin {
		return nil, invalidOperation("AWS-owned documents can't be updated.")
	}

	var out *api.DocumentDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		versions, loadErr := loadDocVersions(tx, m.Scope, rec)
		if loadErr != nil {
			return loadErr
		}

		latest := versions[len(versions)-1]
		if base := value(
			in.DocumentVersion,
			latestSelector,
		); base != latestSelector &&
			base != strconv.FormatInt(latest.Version, 10) {
			return invalidDocumentVersion(base)
		}

		v, newErr := newVersion(in.Content, in.DocumentFormat, in.DisplayName, in.VersionName, in.TargetType,
			rec.Latest+1, s.now().UnixMilli())
		if newErr != nil {
			return newErr
		}

		if _, parseErr := parseContent(v.Content, v.Format, rec.Type); parseErr != nil {
			return parseErr
		}

		if len(versions) >= maxDocVersions {
			return awserror.New("DocumentVersionLimitExceeded", "The document has the most versions it can have.")
		}

		if v.Content == latest.Content && v.Format == latest.Format {
			return awserror.New("DuplicateDocumentContent", "The content of the latest document version is the same.")
		}

		if v.VersionName != "" &&
			slices.ContainsFunc(versions, func(o *docVersion) bool { return o.VersionName == v.VersionName }) {
			return awserror.New("DuplicateDocumentVersionName", "The version name is already used by another version.")
		}

		rec.Latest = v.Version
		if writeErr := storage.Write(tx, docVersionKey(m.Scope, name, v.Version), v); writeErr != nil {
			return writeErr
		}

		if writeErr := storage.Write(tx, docKey(m.Scope, name), rec); writeErr != nil {
			return writeErr
		}

		var descErr error
		out, descErr = describeDocument(rec, v)

		return descErr
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDocumentResult{DocumentDescription: out}, nil
}

func (s *Service) DeleteDocument(
	ctx context.Context,
	in *api.DeleteDocumentRequest,
) (*api.DeleteDocumentResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := documentRef(in.Name)
	if err != nil {
		return nil, err
	}

	if _, _, isBuiltin := builtin(name); isBuiltin {
		return nil, invalidOperation("AWS-owned documents can't be deleted.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if in.DocumentVersion == nil && in.VersionName == nil {
			for _, k := range tx.Keys(docVersionPrefix(m.Scope, name)) {
				tx.Delete(k)
			}

			tx.Delete(docKey(m.Scope, name))

			return nil
		}

		versions, loadErr := loadDocVersions(tx, m.Scope, rec)
		if loadErr != nil {
			return loadErr
		}

		v, pickErr := pickVersion(rec, versions, in.DocumentVersion, in.VersionName)
		if pickErr != nil {
			return pickErr
		}

		if v.Version == rec.Default {
			return invalidOperation("The default version of a document can't be deleted. Change the default " +
				"version first, or delete the whole document.")
		}

		tx.Delete(docVersionKey(m.Scope, name, v.Version))
		rec.Latest = 0
		for _, o := range versions {
			if o.Version != v.Version {
				rec.Latest = max(rec.Latest, o.Version)
			}
		}

		return storage.Write(tx, docKey(m.Scope, name), rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDocumentResult{}, nil
}

func (s *Service) ListDocumentVersions(
	ctx context.Context,
	in *api.ListDocumentVersionsRequest,
) (*api.ListDocumentVersionsResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListDocumentVersionsResult{DocumentVersions: []api.DocumentVersionInfo{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, refErr := documentRef(in.Name)
		if refErr != nil {
			return refErr
		}

		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		versions, loadErr := loadDocVersions(tx, m.Scope, rec)
		if loadErr != nil {
			return loadErr
		}

		selected, next, pageErr := page(versions, func(v *docVersion) string { return fmt.Sprintf("%010d", v.Version) },
			in.NextToken, in.MaxResults, maxVersionsPage)
		if pageErr != nil {
			return pageErr
		}

		for _, v := range selected {
			out.DocumentVersions = append(out.DocumentVersions, api.DocumentVersionInfo{
				Name: new(
					rec.Name,
				),
				DisplayName:     optional(v.DisplayName),
				DocumentVersion: new(strconv.FormatInt(v.Version, 10)),
				VersionName:     optional(v.VersionName),
				CreatedDate:     seconds(v.CreatedAt),
				IsDefaultVersion: new(
					v.Version == rec.Default,
				),
				DocumentFormat: new(v.Format),
				Status:         new(documentActive),
			})
		}

		out.NextToken = next

		return nil
	})

	return out, err
}

func (s *Service) UpdateDocumentDefaultVersion(
	ctx context.Context,
	in *api.UpdateDocumentDefaultVersionRequest,
) (*api.UpdateDocumentDefaultVersionResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := documentRef(in.Name)
	if err != nil {
		return nil, err
	}

	if _, _, isBuiltin := builtin(name); isBuiltin {
		return nil, invalidOperation("AWS-owned documents can't be changed.")
	}

	number, parseErr := strconv.ParseInt(value(in.DocumentVersion, ""), 10, 64)
	if parseErr != nil || number < 1 {
		return nil, invalidDocumentVersion(value(in.DocumentVersion, ""))
	}

	out := &api.DocumentDefaultVersionDescription{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		v, found, readErr := storage.Read[docVersion](tx, docVersionKey(m.Scope, name, number))
		if readErr != nil {
			return readErr
		}

		if !found {
			return invalidDocumentVersion(strconv.FormatInt(number, 10))
		}

		rec.Default = number
		out.Name, out.DefaultVersion, out.DefaultVersionName = new(
			name,
		), new(
			strconv.FormatInt(number, 10),
		), optional(
			v.VersionName,
		)

		return storage.Write(tx, docKey(m.Scope, name), rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDocumentDefaultVersionResult{Description: out}, nil
}

// checkPermissionType requires the Share permission type.
func checkPermissionType(t *string) error {
	if value(t, "") != permissionShare {
		return awserror.New("InvalidPermissionType", "The permission type must be Share.")
	}

	return nil
}

func (s *Service) DescribeDocumentPermission(
	ctx context.Context,
	in *api.DescribeDocumentPermissionRequest,
) (*api.DescribeDocumentPermissionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if typeErr := checkPermissionType(in.PermissionType); typeErr != nil {
		return nil, typeErr
	}

	out := &api.DescribeDocumentPermissionResponse{
		AccountIds:             []string{},
		AccountSharingInfoList: []api.AccountSharingInfo{},
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, refErr := documentRef(in.Name)
		if refErr != nil {
			return refErr
		}

		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		accounts := sortedKeys(rec.Shared)
		selected, next, pageErr := page(
			accounts,
			func(a string) string { return a },
			in.NextToken,
			in.MaxResults,
			maxShared,
		)
		if pageErr != nil {
			return pageErr
		}

		for _, a := range selected {
			out.AccountIds = append(out.AccountIds, a)
			out.AccountSharingInfoList = append(out.AccountSharingInfoList,
				api.AccountSharingInfo{AccountId: new(a), SharedDocumentVersion: new(rec.Shared[a])})
		}

		out.NextToken = next

		return nil
	})

	return out, err
}

var accountID = regexp.MustCompile(`^\d{12}$`)

func (s *Service) ModifyDocumentPermission(
	ctx context.Context,
	in *api.ModifyDocumentPermissionRequest,
) (*api.ModifyDocumentPermissionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if typeErr := checkPermissionType(in.PermissionType); typeErr != nil {
		return nil, typeErr
	}

	name, err := documentRef(in.Name)
	if err != nil {
		return nil, err
	}

	for _, a := range slices.Concat(in.AccountIdsToAdd, in.AccountIdsToRemove) {
		if a != sharedWithAll && !accountID.MatchString(a) {
			return nil, validation("Account IDs must be 12 digits, or all.")
		}
	}

	shared := value(in.SharedDocumentVersion, defaultShare)
	if _, convErr := strconv.ParseInt(
		shared,
		10,
		64,
	); convErr != nil && shared != defaultShare &&
		shared != latestSelector {
		return nil, invalidDocumentVersion(shared)
	}

	if _, _, isBuiltin := builtin(name); isBuiltin {
		return nil, invalidOperation("AWS-owned documents can't be shared.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, loadErr := loadDocument(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if rec.Shared == nil {
			rec.Shared = map[string]string{}
		}

		for _, a := range in.AccountIdsToRemove {
			delete(rec.Shared, a)
		}

		for _, a := range in.AccountIdsToAdd {
			rec.Shared[a] = shared
		}

		if len(rec.Shared) > maxShared {
			return awserror.New("DocumentPermissionLimit", "A document can be shared with at most 20 accounts.")
		}

		return storage.Write(tx, docKey(m.Scope, name), rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyDocumentPermissionResponse{}, nil
}

// documentFilter reports whether a document matches one ListDocuments filter.
type documentFilter func(rec *docRecord, v *docVersion, c *content) bool

// parseDocumentFilters compiles the Filters and the legacy DocumentFilterList
// of ListDocuments. The Owner filter is separate: it widens the candidates.
func parseDocumentFilters(in *api.ListDocumentsRequest, account string) (
	owners []string,
	filters []documentFilter,
	err error,
) {
	pairs := make([]api.DocumentKeyValuesFilter, 0, len(in.Filters)+len(in.DocumentFilterList))
	pairs = append(pairs, in.Filters...)
	for _, f := range in.DocumentFilterList {
		pairs = append(pairs, api.DocumentKeyValuesFilter{Key: f.Key, Values: []string{value(f.Value, "")}})
	}

	for _, f := range pairs {
		values := f.Values
		switch key := value(f.Key, ""); key {
		case "Owner":
			owners = append(owners, values...)
		case "Name":
			filters = append(filters, func(rec *docRecord, _ *docVersion, _ *content) bool {
				return slices.ContainsFunc(values, func(want string) bool { return strings.Contains(rec.Name, want) })
			})
		case "DocumentType":
			filters = append(filters, func(rec *docRecord, _ *docVersion, _ *content) bool {
				return slices.Contains(values, rec.Type)
			})
		case "PlatformTypes":
			filters = append(filters, func(_ *docRecord, _ *docVersion, c *content) bool {
				return slices.ContainsFunc(c.platforms(), func(p string) bool { return slices.Contains(values, p) })
			})
		case "TagKey":
			filters = append(filters, func(rec *docRecord, _ *docVersion, _ *content) bool {
				return slices.ContainsFunc(values, func(k string) bool { _, ok := rec.Tags[k]; return ok })
			})
		default:
			return nil, nil, awserror.New("InvalidFilterKey", "The following filter key is not valid: "+key+
				". Valid keys are Name, Owner, PlatformTypes, DocumentType, and TagKey.")
		}
	}

	_ = account

	return owners, filters, nil
}

// ownedBy reports whether a document with owner is in the Owner filter
// values. No values lists the caller's documents and AWS's.
func ownedBy(owners []string, owner, account string) bool {
	if len(owners) == 0 {
		return true
	}

	return slices.ContainsFunc(owners, func(o string) bool {
		switch o {
		case "Self", "Private":
			return owner == account
		case ownerAmazon, "Public":
			return owner == ownerAmazon
		case "ThirdParty":
			return false
		}

		return o == owner
	})
}

func (s *Service) ListDocuments(ctx context.Context, in *api.ListDocumentsRequest) (*api.ListDocumentsResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	owners, filters, err := parseDocumentFilters(in, m.AccountID)
	if err != nil {
		return nil, err
	}

	type listed struct {
		rec *docRecord
		v   *docVersion
		c   *content
	}

	out := &api.ListDocumentsResult{DocumentIdentifiers: []api.DocumentIdentifier{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found []listed
		add := func(rec *docRecord, v *docVersion) error {
			c, parseErr := parseContent(v.Content, v.Format, rec.Type)
			if parseErr != nil {
				return parseErr
			}

			if ownedBy(owners, rec.Owner, m.AccountID) &&
				!slices.ContainsFunc(filters, func(f documentFilter) bool { return !f(rec, v, c) }) {
				found = append(found, listed{rec: rec, v: v, c: c})
			}

			return nil
		}

		for name := range builtins {
			rec, v, _ := builtin(name)
			if addErr := add(rec, v); addErr != nil {
				return addErr
			}
		}

		prefix := docKey(m.Scope, "")
		for _, k := range tx.Keys(prefix) {
			rec, _, readErr := storage.Read[docRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			v, _, readErr := storage.Read[docVersion](tx, docVersionKey(m.Scope, rec.Name, rec.Default))
			if readErr != nil {
				return readErr
			}

			if addErr := add(rec, v); addErr != nil {
				return addErr
			}
		}

		slices.SortFunc(found, func(a, b listed) int { return strings.Compare(a.rec.Name, b.rec.Name) })

		selected, next, pageErr := page(found, func(l listed) string { return l.rec.Name }, in.NextToken, in.MaxResults,
			maxDocListing)
		if pageErr != nil {
			return pageErr
		}

		for _, l := range selected {
			id := api.DocumentIdentifier{
				Name: new(l.rec.Name), CreatedDate: seconds(l.v.CreatedAt), DisplayName: optional(l.v.DisplayName),
				Owner: new(l.rec.Owner), VersionName: optional(l.v.VersionName),
				DocumentVersion: new(strconv.FormatInt(l.v.Version, 10)), DocumentType: new(l.rec.Type),
				SchemaVersion: optional(l.c.Schema), DocumentFormat: new(l.v.Format),
				TargetType: optional(l.v.TargetType), Tags: tagList(l.rec.Tags),
			}
			if l.rec.Type == typeCommandDoc {
				id.PlatformTypes = l.c.platforms()
			}

			if l.rec.Owner == ownerAmazon {
				id.Author = new(ownerAmazon)
			}

			out.DocumentIdentifiers = append(out.DocumentIdentifiers, id)
		}

		out.NextToken = next

		return nil
	})

	return out, err
}
