package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
)

type instanceIdentityDocument struct {
	AccountID               string  `json:"accountId"`
	Architecture            string  `json:"architecture"`
	AvailabilityZone        string  `json:"availabilityZone"`
	BillingProducts         *string `json:"billingProducts"`
	DevpayProductCodes      *string `json:"devpayProductCodes"`
	MarketplaceProductCodes *string `json:"marketplaceProductCodes"`
	ImageID                 string  `json:"imageId"`
	InstanceID              string  `json:"instanceId"`
	InstanceType            string  `json:"instanceType"`
	KernelID                *string `json:"kernelId"`
	PendingTime             string  `json:"pendingTime"`
	PrivateIP               string  `json:"privateIp"`
	RamdiskID               *string `json:"ramdiskId"`
	Region                  string  `json:"region"`
	Version                 string  `json:"version"`
}

type iamInfoResponse struct {
	Code               string `json:"Code"`
	LastUpdated        string `json:"LastUpdated"`
	InstanceProfileArn string `json:"InstanceProfileArn"`
	InstanceProfileID  string `json:"InstanceProfileId"`
}

type iamCredentialsResponse struct {
	Code            string `json:"Code"`
	LastUpdated     string `json:"LastUpdated"`
	Type            string `json:"Type"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func instanceHostname(region, ip string) string {
	dashed := strings.ReplaceAll(ip, ".", "-")
	if region == "us-east-1" {
		return fmt.Sprintf("ip-%s.ec2.internal", dashed)
	}

	return fmt.Sprintf("ip-%s.%s.compute.internal", dashed, region)
}

func (s *Service) serveIMDS(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	path = strings.TrimPrefix(path, "_tinystack/imds/")
	path = strings.TrimPrefix(path, "_tinystack/imds")

	// Parse optional instance ID prefix: /<instance-id>/latest/...
	var targetInstanceID string
	if before, after, ok := strings.Cut(path, "/latest"); ok && !strings.HasPrefix(path, "latest") {
		targetInstanceID = before
		path = "latest" + after
	}

	if r.Method == http.MethodPut && (path == "latest/api/token" || path == "latest/api/token/") {
		s.issueIMDSToken(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasPrefix(path, "latest/") && path != "latest" {
		http.NotFound(w, r)
		return
	}

	subpath := strings.TrimPrefix(path, "latest/")
	if path == "latest" {
		subpath = ""
	}

	// Resolve the EC2 instance
	if s.verifyIdentity && strings.HasPrefix(r.URL.Path, "/_tinystack/imds/") &&
		strings.HasPrefix(subpath, "meta-data/iam/security-credentials") {
		http.Error(
			w,
			"credential paths require the dedicated IMDS listener in identity.mode verify",
			http.StatusForbidden,
		)
		return
	}

	inst, scope, found, err := s.findInstance(r.Context(), targetInstanceID, r.Header)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !found {
		http.Error(
			w,
			"no unique running instance; select X-TinyStack-Instance-Id or metadata.imds.default_instance_id",
			http.StatusNotFound,
		)
		return
	}

	if !inst.EndpointEnabled {
		http.NotFound(w, r)
		return
	}

	// Check IMDSv1 vs IMDSv2 token rules
	tokenHeader := r.Header.Get("X-aws-ec2-metadata-token")
	if tokenHeader != "" {
		if !s.validateIMDSToken(tokenHeader) {
			http.Error(w, "unauthorized: invalid or expired metadata token", http.StatusUnauthorized)
			return
		}
	} else if inst.TokensRequired {
		http.Error(w, "unauthorized: token required", http.StatusUnauthorized)
		return
	}

	s.serveIMDSPath(w, r, inst, scope, subpath)
}

func (s *Service) issueIMDSToken(w http.ResponseWriter, r *http.Request) {
	ttlStr := r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds")
	if ttlStr == "" {
		http.Error(w, "missing required header X-aws-ec2-metadata-token-ttl-seconds", http.StatusBadRequest)
		return
	}

	ttl, err := strconv.Atoi(ttlStr)
	if err != nil || ttl < 1 || ttl > 21600 {
		http.Error(
			w,
			"invalid X-aws-ec2-metadata-token-ttl-seconds: must be between 1 and 21600",
			http.StatusBadRequest,
		)
		return
	}

	token, err := s.generateToken()
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	expires := s.now().Add(time.Duration(ttl) * time.Second)

	s.imdsMu.Lock()
	// Expired tokens are dropped as new ones are issued, so the set stays bounded.
	for t, at := range s.imdsTokens {
		if !s.now().Before(at) {
			delete(s.imdsTokens, t)
		}
	}

	s.imdsTokens[token] = expires
	s.imdsMu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-aws-ec2-metadata-token-ttl-seconds", strconv.Itoa(ttl))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(token))
}

func (s *Service) validateIMDSToken(token string) bool {
	if token == "" {
		return false
	}

	s.imdsMu.RLock()
	expires, ok := s.imdsTokens[token]
	s.imdsMu.RUnlock()

	if !ok || !s.now().Before(expires) {
		return false
	}

	return true
}

func (s *Service) findInstance(ctx context.Context, explicitID string, headers http.Header) (
	inst *ec2.InstanceMetadata, scope request.Scope, found bool, err error,
) {
	id := explicitID
	if id == "" {
		id = headers.Get("X-TinyStack-Instance-Id")
	}

	if id == "" {
		id = s.defaultInstanceID
	}

	inst, found, err = s.instances.MetadataInstance(ctx, id)
	if err != nil || !found {
		return nil, request.Scope{}, false, err
	}

	return inst, inst.Scope, true, nil
}

//nolint:gosec // G705: IMDS metadata is explicitly served as text/plain, as required by its wire protocol.
func (s *Service) serveIMDSPath(
	w http.ResponseWriter,
	r *http.Request,
	inst *ec2.InstanceMetadata,
	scope request.Scope,
	subpath string,
) {
	subpath = strings.TrimPrefix(subpath, "/")

	switch {
	case subpath == "" || subpath == "meta-data" || subpath == "meta-data/":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(
			w,
			"ami-id\nhostname\niam/\ninstance-id\ninstance-type\nlocal-ipv4\nmac\nnetwork/\nplacement/\n",
		)

	case subpath == "meta-data/instance-id":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, inst.ID)

	case subpath == "meta-data/instance-type":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, inst.Type)

	case subpath == "meta-data/ami-id":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, inst.ImageID)

	case subpath == "meta-data/hostname":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, instanceHostname(scope.Region, inst.PrivateIP))

	case subpath == "meta-data/local-ipv4":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, inst.PrivateIP)

	case subpath == "meta-data/placement" || subpath == "meta-data/placement/":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "availability-zone\nregion\n")

	case subpath == "meta-data/placement/availability-zone":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, inst.Zone)

	case subpath == "meta-data/placement/region":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, scope.Region)

	case subpath == "meta-data/mac":
		mac := inst.MAC
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, mac)

	case subpath == "meta-data/network/interfaces/macs" || subpath == "meta-data/network/interfaces/macs/":
		mac := inst.MAC
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%s/\n", mac)

	case strings.HasPrefix(subpath, "meta-data/network/interfaces/macs/"):
		s.serveNetworkInterface(w, r, inst, scope, strings.TrimPrefix(subpath, "meta-data/network/interfaces/macs/"))

	case subpath == "meta-data/iam/info":
		s.serveIAMInfo(w, r, inst)

	case subpath == "meta-data/iam/security-credentials" || subpath == "meta-data/iam/security-credentials/":
		s.serveIAMSecurityCredentialsList(w, r, inst, scope)

	case strings.HasPrefix(subpath, "meta-data/iam/security-credentials/"):
		roleName := strings.TrimPrefix(subpath, "meta-data/iam/security-credentials/")
		s.serveIAMSecurityCredentialsRole(w, r, inst, scope, roleName)

	case subpath == "dynamic/instance-identity/document":
		s.serveInstanceIdentityDocument(w, r, inst, scope)

	case subpath == "user-data":
		s.serveUserData(w, r, inst)

	default:
		http.NotFound(w, r)
	}
}

//nolint:gosec // G705: IMDS metadata is explicitly served as text/plain, as required by its wire protocol.
func (s *Service) serveNetworkInterface(
	w http.ResponseWriter,
	r *http.Request,
	inst *ec2.InstanceMetadata,
	scope request.Scope,
	rest string,
) {
	requestedMAC, _, _ := strings.Cut(rest, "/")
	if requestedMAC != inst.MAC {
		http.NotFound(w, r)
		return
	}

	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		// e.g. <mac>
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(
			w,
			"device-number\ninterface-id\nlocal-ipv4s\nmac\nowner-id\nsubnet-id\nvpc-id\nvpc-ipv4-cidr-block\n",
		)
		return
	}

	mac := rest[:slashIdx]
	item := strings.TrimPrefix(rest[slashIdx+1:], "/")

	eniID := inst.InterfaceID

	w.Header().Set("Content-Type", "text/plain")
	switch item {
	case "", "/":
		_, _ = fmt.Fprint(
			w,
			"device-number\ninterface-id\nlocal-ipv4s\nmac\nowner-id\nsubnet-id\nvpc-id\nvpc-ipv4-cidr-block\n",
		)
	case "device-number":
		_, _ = fmt.Fprint(w, "0")
	case "interface-id":
		_, _ = fmt.Fprint(w, eniID)
	case "local-ipv4s":
		_, _ = fmt.Fprint(w, inst.PrivateIP)
	case "mac":
		_, _ = fmt.Fprint(w, mac)
	case "owner-id":
		_, _ = fmt.Fprint(w, scope.AccountID)
	case "subnet-id":
		_, _ = fmt.Fprint(w, inst.SubnetID)
	case "vpc-id":
		_, _ = fmt.Fprint(w, inst.VPCID)
	case "vpc-ipv4-cidr-block":
		_, _ = fmt.Fprint(w, inst.VPCCIDR)
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) serveIAMInfo(w http.ResponseWriter, r *http.Request, inst *ec2.InstanceMetadata) {
	if inst.ProfileARN == "" {
		http.NotFound(w, r)
		return
	}

	resp := iamInfoResponse{
		Code:               "Success",
		LastUpdated:        s.now().UTC().Format(time.RFC3339),
		InstanceProfileArn: inst.ProfileARN,
		InstanceProfileID:  inst.ProfileID,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Service) getProfileRoleARN(
	ctx context.Context,
	scope request.Scope,
	inst *ec2.InstanceMetadata,
) (roleARN, roleName string, err error) {
	if inst.ProfileARN == "" {
		return "", "", nil
	}

	if s.instanceProfiles == nil {
		return "", "", fmt.Errorf("IAM instance profile lookup is not configured")
	}

	prof, ok, err := s.instanceProfiles(ctx, scope.AccountID, inst.ProfileARN)
	if err != nil {
		return "", "", err
	}

	if !ok || prof.RoleARN == "" {
		return "", "", nil
	}

	roleName = prof.RoleARN[strings.LastIndex(prof.RoleARN, "/")+1:]
	return prof.RoleARN, roleName, nil
}

//nolint:gosec // G705: IMDS metadata is explicitly served as text/plain, as required by its wire protocol.
func (s *Service) serveIAMSecurityCredentialsList(
	w http.ResponseWriter,
	r *http.Request,
	inst *ec2.InstanceMetadata,
	scope request.Scope,
) {
	_, roleName, err := s.getProfileRoleARN(r.Context(), scope, inst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if roleName == "" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprint(w, roleName)
}

func (s *Service) serveIAMSecurityCredentialsRole(
	w http.ResponseWriter,
	r *http.Request,
	inst *ec2.InstanceMetadata,
	scope request.Scope,
	requestedRole string,
) {
	roleARN, roleName, err := s.getProfileRoleARN(r.Context(), scope, inst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if roleName == "" || roleName != requestedRole {
		http.NotFound(w, r)
		return
	}

	if s.assumeRole == nil {
		http.Error(w, "STS not configured", http.StatusInternalServerError)
		return
	}

	accessKey, secret, token, expires, err := s.assumeRole(
		r.Context(),
		"ec2.amazonaws.com",
		roleARN,
		roleName,
		3600,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := iamCredentialsResponse{
		Code:            "Success",
		LastUpdated:     s.now().UTC().Format(time.RFC3339),
		Type:            "AWS-HMAC",
		AccessKeyID:     accessKey,
		SecretAccessKey: secret,
		Token:           token,
		Expiration:      expires.UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Service) serveInstanceIdentityDocument(
	w http.ResponseWriter,
	_ *http.Request,
	inst *ec2.InstanceMetadata,
	scope request.Scope,
) {
	doc := instanceIdentityDocument{
		AccountID:        scope.AccountID,
		Architecture:     "x86_64",
		AvailabilityZone: inst.Zone,
		ImageID:          inst.ImageID,
		InstanceID:       inst.ID,
		InstanceType:     inst.Type,
		PendingTime:      inst.LaunchTime.UTC().Format(time.RFC3339),
		PrivateIP:        inst.PrivateIP,
		Region:           scope.Region,
		Version:          "2017-09-30",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

//nolint:gosec // G705: IMDS metadata is explicitly served as text/plain, as required by its wire protocol.
func (s *Service) serveUserData(w http.ResponseWriter, r *http.Request, inst *ec2.InstanceMetadata) {
	if inst.UserData == "" {
		http.NotFound(w, r)
		return
	}

	userData := inst.UserData
	if decoded, err := base64.StdEncoding.DecodeString(userData); err == nil {
		userData = string(decoded)
	}

	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(userData))
}
