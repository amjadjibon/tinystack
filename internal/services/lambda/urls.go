package lambda

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A function URL's configuration is "url/<name>/<qualifier>", where the
// qualifier is an alias or $LATEST for the unqualified function, and the
// partition-wide "aws/lambda/url/<URL ID>" finds it from a request.
const (
	kindURL   = "url"
	urlIndex  = "aws/lambda/url/"
	authNone  = "NONE"
	authIAM   = "AWS_IAM"
	buffered  = "BUFFERED"
	streaming = "RESPONSE_STREAM"
)

// functionURL is a function URL's configuration.
type functionURL struct {
	Cors       *api.Cors `json:",omitempty"`
	ID         string
	Qualifier  string
	AuthType   string
	InvokeMode string
	Created    string
	Modified   string
}

// urlTarget is the function a URL ID belongs to.
type urlTarget struct {
	Account   string
	Region    string
	Name      string
	Qualifier string
}

func urlKey(scope request.Scope, name, qualifier string) string {
	return scopeKey(scope, kindURL, name+"/"+qualifier)
}

// newURLID is a URL ID: 32 lowercase letters and digits.
func newURLID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [32]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}

	return string(b[:])
}

// urlEndpoint is the URL a function URL is called at: a subdomain of the host
// the client used, <ID>.lambda-url.<region>.<host>, where an IP address becomes
// "localhost", whose subdomains resolve to the loopback address.
func urlEndpoint(base, id, region string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "http://" + id + ".lambda-url." + region + ".localhost/"
	}

	host, port := u.Hostname(), u.Port()
	if net.ParseIP(host) != nil {
		host = "localhost"
	}

	if port != "" {
		host = net.JoinHostPort(host, port)
	}

	return u.Scheme + "://" + id + ".lambda-url." + region + "." + host + "/"
}

// checkURL validates a URL configuration's auth type, invoke mode, and CORS.
func checkURL(auth, mode *string, cors *api.Cors) error {
	if a := common.Value(auth); a != authNone && a != authIAM {
		return invalid("AuthType must be NONE or AWS_IAM")
	}

	if m := common.Value(mode); m != "" && m != buffered && m != streaming {
		return invalid("InvokeMode must be BUFFERED or RESPONSE_STREAM")
	}

	if cors == nil {
		return nil
	}

	if common.Value(cors.MaxAge) < 0 || common.Value(cors.MaxAge) > 86400 {
		return invalid("Cors MaxAge must be from 0 to 86400")
	}

	for _, m := range cors.AllowMethods {
		if m != "*" && !corsMethods[strings.ToUpper(m)] {
			return invalid("Cors AllowMethods has an unsupported method %s", m)
		}
	}

	return nil
}

var corsMethods = map[string]bool{
	http.MethodGet: true, http.MethodPut: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// urlFunction resolves the function and qualifier a URL request names. The
// qualifier must be an alias, or none for the unqualified function.
func urlFunction(tx storage.ReadTx, m *request.Metadata, raw, qualifier *string) (name, q string, err error) {
	if name, q, err = resolveName(*m, common.Value(raw)); err != nil {
		return "", "", err
	}

	if q, err = withQualifier(q, qualifier); err != nil {
		return "", "", err
	}

	if q == "" {
		q = latest
	}

	if versionNumber.MatchString(q) {
		return "", "", invalid("Function URLs can be created only for $LATEST or an alias")
	}

	_, _, err = resolve(tx, m.Scope, name, q)
	return name, q, err
}

// urlARN is the function ARN a URL configuration reports.
func urlARN(scope request.Scope, name, qualifier string) string {
	if qualifier == latest {
		return functionARN(scope, name)
	}

	return qualifiedARN(scope, name, qualifier)
}

func (u *functionURL) config(m *request.Metadata, name string) api.FunctionUrlConfig {
	return api.FunctionUrlConfig{
		FunctionUrl: new(urlEndpoint(m.Endpoint, u.ID, m.Region)), FunctionArn: new(urlARN(m.Scope, name, u.Qualifier)),
		AuthType: new(u.AuthType), Cors: u.Cors, InvokeMode: new(u.InvokeMode), CreationTime: new(u.Created),
		LastModifiedTime: new(u.Modified),
	}
}

func (s *Service) CreateFunctionUrlConfig(
	ctx context.Context,
	in *api.CreateFunctionUrlConfigRequest,
) (*api.CreateFunctionUrlConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkURL(in.AuthType, in.InvokeMode, in.Cors); checkErr != nil {
		return nil, checkErr
	}

	now := s.now().UTC().Format(timeFormat)
	u := functionURL{
		ID: newURLID(), AuthType: *in.AuthType, Cors: in.Cors, InvokeMode: common.Value(in.InvokeMode),
		Created: now, Modified: now,
	}
	if u.InvokeMode == "" {
		u.InvokeMode = buffered
	}

	var name string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var resolveErr error
		if name, u.Qualifier, resolveErr = urlFunction(tx, &m, in.FunctionName, in.Qualifier); resolveErr != nil {
			return resolveErr
		}

		key := urlKey(m.Scope, name, u.Qualifier)
		if _, exists := tx.Get(key); exists {
			return conflict("Failed to create function url config for [functionArn = " +
				urlARN(m.Scope, name, u.Qualifier) + "]. Error message:  FunctionUrlConfig exists for this " +
				"Lambda function")
		}

		target := urlTarget{Account: m.AccountID, Region: m.Region, Name: name, Qualifier: u.Qualifier}
		if writeErr := writeJSON(tx, urlIndex+u.ID, &target); writeErr != nil {
			return writeErr
		}

		return writeJSON(tx, key, &u)
	})
	if err != nil {
		return nil, err
	}

	c := u.config(&m, name)
	return &api.CreateFunctionUrlConfigResponse{
		FunctionUrl: c.FunctionUrl, FunctionArn: c.FunctionArn, AuthType: c.AuthType, Cors: c.Cors,
		CreationTime: c.CreationTime, InvokeMode: c.InvokeMode,
	}, nil
}

// readURL loads the URL configuration a request names.
func readURL(tx storage.ReadTx, m *request.Metadata, raw, qualifier *string) (string, *functionURL, error) {
	name, q, err := urlFunction(tx, m, raw, qualifier)
	if err != nil {
		return "", nil, err
	}

	var u functionURL
	found, err := readJSON(tx, urlKey(m.Scope, name, q), &u)
	if err == nil && !found {
		err = notFound("The resource you requested does not exist.")
	}

	return name, &u, err
}

func (s *Service) GetFunctionUrlConfig(
	ctx context.Context,
	in *api.GetFunctionUrlConfigRequest,
) (*api.GetFunctionUrlConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c api.FunctionUrlConfig
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, u, readErr := readURL(tx, &m, in.FunctionName, in.Qualifier)
		if readErr == nil {
			c = u.config(&m, name)
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetFunctionUrlConfigResponse{
		FunctionUrl: c.FunctionUrl, FunctionArn: c.FunctionArn, AuthType: c.AuthType, Cors: c.Cors,
		CreationTime: c.CreationTime, LastModifiedTime: c.LastModifiedTime, InvokeMode: c.InvokeMode,
	}, nil
}

// UpdateFunctionUrlConfig changes the settings a request gives; the others
// keep their values.
func (s *Service) UpdateFunctionUrlConfig(
	ctx context.Context,
	in *api.UpdateFunctionUrlConfigRequest,
) (*api.UpdateFunctionUrlConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c api.FunctionUrlConfig
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, u, readErr := readURL(tx, &m, in.FunctionName, in.Qualifier)
		if readErr != nil {
			return readErr
		}

		if in.AuthType != nil {
			u.AuthType = *in.AuthType
		}

		if in.InvokeMode != nil {
			u.InvokeMode = *in.InvokeMode
		}

		if in.Cors != nil {
			u.Cors = in.Cors
		}

		if checkErr := checkURL(&u.AuthType, &u.InvokeMode, u.Cors); checkErr != nil {
			return checkErr
		}

		u.Modified = s.now().UTC().Format(timeFormat)
		c = u.config(&m, name)
		return writeJSON(tx, urlKey(m.Scope, name, u.Qualifier), u)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateFunctionUrlConfigResponse{
		FunctionUrl: c.FunctionUrl, FunctionArn: c.FunctionArn, AuthType: c.AuthType, Cors: c.Cors,
		CreationTime: c.CreationTime, LastModifiedTime: c.LastModifiedTime, InvokeMode: c.InvokeMode,
	}, nil
}

func (s *Service) DeleteFunctionUrlConfig(
	ctx context.Context,
	in *api.DeleteFunctionUrlConfigRequest,
) (*api.DeleteFunctionUrlConfigOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, u, readErr := readURL(tx, &m, in.FunctionName, in.Qualifier)
		if readErr != nil {
			return readErr
		}

		deleteURL(tx, m.Scope, name, u)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionUrlConfigOutput{}, nil
}

func deleteURL(tx storage.WriteTx, scope request.Scope, name string, u *functionURL) {
	tx.Delete(urlKey(scope, name, u.Qualifier))
	tx.Delete(urlIndex + u.ID)
}

// deleteURLs deletes the URL configurations of a function, or of one of its
// aliases when qualifier is not empty.
func deleteURLs(tx storage.WriteTx, scope request.Scope, name, qualifier string) error {
	for _, k := range tx.Keys(urlKey(scope, name, qualifier)) {
		var u functionURL
		if _, err := readJSON(tx, k, &u); err != nil {
			return err
		}

		if qualifier == "" || u.Qualifier == qualifier {
			deleteURL(tx, scope, name, &u)
		}
	}

	return nil
}

func (s *Service) ListFunctionUrlConfigs(
	ctx context.Context,
	in *api.ListFunctionUrlConfigsRequest,
) (*api.ListFunctionUrlConfigsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, start, err := layerPage(in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListFunctionUrlConfigsResponse{FunctionUrlConfigs: []api.FunctionUrlConfig{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		for _, k := range tx.Keys(urlKey(m.Scope, name, "")) {
			var u functionURL
			if _, readErr := readJSON(tx, k, &u); readErr != nil {
				return readErr
			}

			if u.Qualifier <= start {
				continue
			}

			if int64(len(out.FunctionUrlConfigs)) == limit {
				out.NextMarker = nextMarker(start)
				return nil
			}

			out.FunctionUrlConfigs = append(out.FunctionUrlConfigs, u.config(&m, name))
			start = u.Qualifier
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// URLs serves function URLs. It is the execute.Handler the gateway calls for
// requests to <URL ID>.lambda-url.<region>.<host> and /_aws/lambda-url/<URL ID>/.
func (s *Service) URLs() execute.Handler { return urls{s} }

type urls struct{ s *Service }

// allowedOrigin returns the Access-Control-Allow-Origin a CORS configuration
// gives origin, if any.
func allowedOrigin(c *api.Cors, origin string) (string, bool) {
	for _, o := range c.AllowOrigins {
		if o == "*" {
			return "*", true
		}

		if strings.EqualFold(o, origin) {
			return origin, true
		}
	}

	return "", false
}

// corsHeaders are the CORS headers of a response to a request from origin;
// a preflight also gets the allowed methods, headers, and max age.
func corsHeaders(c *api.Cors, origin string, preflight bool) http.Header {
	h := http.Header{}
	if c == nil || origin == "" {
		return h
	}

	allowed, ok := allowedOrigin(c, origin)
	if !ok {
		return h
	}

	h.Set("Access-Control-Allow-Origin", allowed)
	if common.Value(c.AllowCredentials) {
		h.Set("Access-Control-Allow-Credentials", "true")
	}

	if len(c.ExposeHeaders) > 0 && !preflight {
		h.Set("Access-Control-Expose-Headers", strings.Join(c.ExposeHeaders, ","))
	}

	if preflight {
		if len(c.AllowMethods) > 0 {
			h.Set("Access-Control-Allow-Methods", strings.Join(c.AllowMethods, ","))
		}

		if len(c.AllowHeaders) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(c.AllowHeaders, ","))
		}

		if c.MaxAge != nil {
			h.Set("Access-Control-Max-Age", strconv.FormatInt(*c.MaxAge, 10))
		}
	}

	return h
}

// signed reports whether a request carries a SigV4 signature, in its
// Authorization header or, presigned, in its query.
func signed(r *execute.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") ||
		r.Query.Get("X-Amz-Signature") != ""
}

// Execute runs a request to a function URL: it answers CORS preflights from
// the URL's CORS configuration and otherwise invokes the function with a
// payload format 2.0 event, mapping its response as HTTP APIs do.
func (h urls) Execute(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	s := h.s
	var target urlTarget
	var u functionURL
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		found, readErr := readJSON(tx, urlIndex+id, &target)
		if readErr != nil || !found {
			return readErr
		}

		scope := request.Scope{AccountID: target.Account, Region: target.Region}
		_, readErr = readJSON(tx, urlKey(scope, target.Name, target.Qualifier), &u)
		return readErr
	})
	if err != nil || target.Name == "" {
		return nil, false, err
	}

	origin := r.Header.Get("Origin")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" && u.Cors != nil {
		return &execute.Response{Status: http.StatusOK, Header: corsHeaders(u.Cors, origin, true)}, true, nil
	}

	if u.AuthType == authIAM && !signed(r) {
		return &execute.Response{
			Status: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
			Body: []byte(`{"Message":"Forbidden"}`),
		}, true, nil
	}

	host, _, splitErr := net.SplitHostPort(r.Host)
	if splitErr != nil {
		host = r.Host
	}

	event, err := proxy.EventV2(r, &proxy.Context{
		AccountID: target.Account, APIID: id, Stage: "$default", RouteKey: "$default", Path: r.Path,
		DomainName: host, RequestID: uuid.NewV4().String(),
	}, nil, nil)
	if err != nil {
		return nil, true, err
	}

	qualifier := target.Qualifier
	if qualifier == latest {
		qualifier = ""
	}

	scope := request.Scope{AccountID: target.Account, Region: target.Region}
	result, _, _, err := s.run(ctx, scope, target.Name, qualifier, event)
	var resp *execute.Response
	switch e, ok := errors.AsType[*awserror.Error](err); {
	case ok && e.Code == "TooManyRequestsException":
		resp = execute.Message(http.StatusTooManyRequests, "Rate Exceeded.")
	case err != nil:
		s.logger.Warn("function URL invocation failed", "url", id, "error", err)
		resp = execute.Message(http.StatusBadGateway, "Internal Server Error")
	case result.Error:
		resp = execute.Message(http.StatusBadGateway, "Internal Server Error")
	default:
		resp = proxy.ResponseV2(result.Payload, false)
	}

	if resp.Header == nil {
		resp.Header = http.Header{}
	}

	// CORS headers the function set itself take precedence.
	for k, v := range corsHeaders(u.Cors, origin, false) {
		if resp.Header.Get(k) == "" {
			resp.Header[k] = v
		}
	}

	return resp, true, nil
}
