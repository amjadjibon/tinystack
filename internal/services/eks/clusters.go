package eks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	platformVersion   = "eks.1"
	defaultServiceNet = "10.100.0.0/16"
	configMap         = "CONFIG_MAP"
	apiAndConfigMap   = "API_AND_CONFIG_MAP"
	apiMode           = "API"
	extended          = "EXTENDED"
)

// kubeVersion is a Kubernetes version TinyStack's clusters can run.
type kubeVersion struct {
	released time.Time
	name     string
	patch    string
}

// kubeVersions are the versions clusters can run, oldest first. Standard
// support lasts 14 months from release and extended support 12 more.
var kubeVersions = []kubeVersion{
	{name: "1.30", patch: "1.30.14", released: time.Date(2024, 5, 23, 0, 0, 0, 0, time.UTC)},
	{name: "1.31", patch: "1.31.10", released: time.Date(2024, 9, 26, 0, 0, 0, 0, time.UTC)},
	{name: "1.32", patch: "1.32.6", released: time.Date(2025, 1, 23, 0, 0, 0, 0, time.UTC)},
	{name: "1.33", patch: "1.33.2", released: time.Date(2025, 5, 29, 0, 0, 0, 0, time.UTC)},
	{name: "1.34", patch: "1.34.1", released: time.Date(2025, 10, 2, 0, 0, 0, 0, time.UTC)},
	{name: "1.35", patch: "1.35.0", released: time.Date(2026, 1, 27, 0, 0, 0, 0, time.UTC)},
}

// defaultVersion is the version a cluster that names none runs.
const defaultVersion = "1.34"

func versionIndex(v string) int {
	return slices.IndexFunc(kubeVersions, func(k kubeVersion) bool { return k.name == v })
}

var logTypes = []string{"api", "audit", "authenticator", "controllerManager", "scheduler"}

type cluster struct {
	Created time.Time
	Network api.KubernetesNetworkConfigResponse
	Tags    map[string]string
	// ZonalShift is reported only once it is configured.
	ZonalShift *bool
	Status     string
	Authority  string
	Version    string
	RoleArn    string
	Name       string
	Token      string
	Endpoint   string
	ID         string
	AuthMode   string
	Support    string
	Vpc        api.VpcConfigResponse
	// Server is the API server the runner started, if any.
	Server             *Server `json:",omitempty"`
	Logging            []string
	Encryption         []api.EncryptionConfig
	Bootstrap          bool
	DeletionProtection bool
}

// hostID is the form of the cluster's ID its endpoint and OIDC issuer use.
func (c *cluster) hostID() string { return strings.ToUpper(strings.ReplaceAll(c.ID, "-", "")) }

func clusterKey(scope request.Scope, name string) string { return prefix(scope, kindCluster) + name }

func clusterARN(scope request.Scope, name string) string { return arn(scope, "cluster/"+name) }

func clusterNotFound(name string) error { return notFound("No cluster found for name: %s.", name) }

// loadCluster returns the cluster name names.
func loadCluster(tx storage.ReadTx, scope request.Scope, name *string) (*cluster, error) {
	c, ok, err := storage.Read[cluster](tx, clusterKey(scope, common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, clusterNotFound(common.Value(name)))
	}

	return c, nil
}

func (c *cluster) output(scope request.Scope) *api.Cluster {
	out := &api.Cluster{
		Name: new(c.Name), Arn: new(clusterARN(scope, c.Name)), CreatedAt: common.SecondsMilli(c.Created),
		Version: new(c.Version), Endpoint: new(c.Endpoint), RoleArn: new(c.RoleArn), ResourcesVpcConfig: &c.Vpc,
		KubernetesNetworkConfig: &c.Network, Logging: logging(c.Logging), Status: new(c.Status),
		Identity: &api.Identity{Oidc: &api.OIDC{
			Issuer: new("https://oidc.eks." + scope.Region + ".amazonaws.com/id/" + c.hostID()),
		}},
		CertificateAuthority: &api.Certificate{Data: new(c.Authority)}, PlatformVersion: new(platformVersion),
		Tags: common.CloneTags(c.Tags), EncryptionConfig: c.Encryption, Id: new(c.ID),
		Health: &api.ClusterHealth{Issues: []api.ClusterIssue{}},
		AccessConfig: &api.AccessConfigResponse{
			AuthenticationMode: new(c.AuthMode), BootstrapClusterCreatorAdminPermissions: new(c.Bootstrap),
		},
		UpgradePolicy:      &api.UpgradePolicyResponse{SupportType: new(c.Support)},
		DeletionProtection: new(c.DeletionProtection),
	}
	if c.Token != "" {
		out.ClientRequestToken = new(c.Token)
	}

	if c.ZonalShift != nil {
		out.ZonalShiftConfig = &api.ZonalShiftConfigResponse{Enabled: c.ZonalShift}
	}

	return out
}

// logging describes the enabled log types and the disabled ones, as EKS does.
func logging(enabled []string) *api.Logging {
	out := &api.Logging{ClusterLogging: []api.LogSetup{}}
	var disabled []string
	for _, t := range logTypes {
		if !slices.Contains(enabled, t) {
			disabled = append(disabled, t)
		}
	}

	if len(enabled) > 0 {
		out.ClusterLogging = append(out.ClusterLogging, api.LogSetup{Types: enabled, Enabled: new(true)})
	}

	if len(disabled) > 0 {
		out.ClusterLogging = append(out.ClusterLogging, api.LogSetup{Types: disabled, Enabled: new(false)})
	}

	return out
}

// applyLogging returns the enabled log types after in's changes.
func applyLogging(enabled []string, in *api.Logging) ([]string, error) {
	for _, setup := range in.ClusterLogging {
		for _, t := range setup.Types {
			if !slices.Contains(logTypes, t) {
				return nil, invalidParameter(
					"Log types are api, audit, authenticator, controllerManager, and scheduler.",
				)
			}

			enabled = slices.DeleteFunc(enabled, func(e string) bool { return e == t })
			if common.Value(setup.Enabled) {
				enabled = append(enabled, t)
			}
		}
	}

	// In EKS's order.
	return slices.DeleteFunc(slices.Clone(logTypes), func(t string) bool { return !slices.Contains(enabled, t) }), nil
}

// authority returns a new self-signed cluster certificate authority, base64
// PEM as EKS reports it.
func authority(now time.Time) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return "", err
	}

	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "kubernetes"}, NotBefore: now,
		NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func checkEndpoints(v *api.VpcConfigRequest, current *api.VpcConfigResponse) error {
	public := common.Value(common.OrElse(v.EndpointPublicAccess, current.EndpointPublicAccess))
	private := common.Value(common.OrElse(v.EndpointPrivateAccess, current.EndpointPrivateAccess))
	if !public && !private {
		return invalidParameter("Cluster endpoint access must be public, private, or both.")
	}

	return nil
}

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterRequest) (*api.CreateClusterResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	c, err := s.newCluster(ctx, scope, in)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, ok, readErr := storage.Read[cluster](tx, clusterKey(scope, c.Name))
		switch {
		case readErr != nil:
			return readErr
		case ok && c.Token != "" && existing.Token == c.Token:
			c = existing
			return nil
		case ok:
			return inUse("Cluster already exists with name: %s", c.Name)
		}

		if serveErr := s.serve(tx, scope, c); serveErr != nil {
			return serveErr
		}

		// The cluster's creator gets an admin access entry.
		if c.Bootstrap && c.AuthMode != configMap {
			creator := "arn:aws:iam::" + scope.AccountID + ":root"
			e := newEntry(scope, c, creator, standard, "", nil, nil, s.now().UTC())
			e.Policies = []api.AssociatedAccessPolicy{{
				PolicyArn: new(policyPrefix + "AmazonEKSClusterAdminPolicy"), AccessScope: &api.AccessScope{
					Type: new(scopeCluster), Namespaces: []string{},
				}, AssociatedAt: e.Entry.CreatedAt, ModifiedAt: e.Entry.CreatedAt,
			}}
			if writeErr := storage.Write(tx, entryKey(scope, c.Name, creator), e); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, clusterKey(scope, c.Name), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterResponse{Cluster: c.output(scope)}, nil
}

// newCluster checks a CreateCluster request and returns the cluster record.
func (s *Service) newCluster(ctx context.Context, scope request.Scope, in *api.CreateClusterRequest) (*cluster, error) {
	if err := checkName(in.Name, "name", 100); err != nil {
		return nil, err
	}

	if err := checkRole(in.RoleArn, "roleArn"); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	version := defaultVersion
	if in.Version != nil {
		if versionIndex(*in.Version) < 0 {
			return nil, invalidParameter("unsupported Kubernetes version %s", *in.Version)
		}

		version = *in.Version
	}

	vpc := common.Value(in.ResourcesVpcConfig)
	if len(vpc.SubnetIds) < 2 {
		return nil, invalidParameter("Subnets specified must be in at least two different AZs")
	}

	current := &api.VpcConfigResponse{EndpointPublicAccess: new(true), EndpointPrivateAccess: new(false)}
	if err := checkEndpoints(&vpc, current); err != nil {
		return nil, err
	}

	placed, err := s.place(ctx, scope, vpc.SubnetIds, vpc.SecurityGroupIds)
	if err != nil {
		return nil, err
	}

	if len(placed.Zones) < 2 {
		return nil, invalidParameter("Subnets specified must be in at least two different AZs")
	}

	c := &cluster{
		Created: s.now().
			UTC(),
		Tags:               common.CloneTags(in.Tags),
		Name:               *in.Name,
		ID:                 uuid.NewV4().String(),
		Version:            version,
		RoleArn:            *in.RoleArn,
		Status:             active,
		Token:              common.Value(in.ClientRequestToken),
		AuthMode:           configMap,
		Support:            extended,
		Bootstrap:          true,
		Encryption:         in.EncryptionConfig,
		DeletionProtection: common.Value(in.DeletionProtection),
		Vpc: api.VpcConfigResponse{
			SubnetIds: vpc.SubnetIds, SecurityGroupIds: orEmpty(vpc.SecurityGroupIds), VpcId: new(placed.VPC),
			EndpointPublicAccess:  common.OrElse(vpc.EndpointPublicAccess, current.EndpointPublicAccess),
			EndpointPrivateAccess: common.OrElse(vpc.EndpointPrivateAccess, current.EndpointPrivateAccess),
			PublicAccessCidrs:     vpc.PublicAccessCidrs,
		},
		Network: api.KubernetesNetworkConfigResponse{ServiceIpv4Cidr: new(defaultServiceNet), IpFamily: new("ipv4")},
	}
	c.Endpoint = "https://" + c.hostID() + ".gr7." + scope.Region + ".eks.amazonaws.com"
	if z := in.ZonalShiftConfig; z != nil {
		c.ZonalShift = new(common.Value(z.Enabled))
	}

	if len(c.Vpc.PublicAccessCidrs) == 0 {
		c.Vpc.PublicAccessCidrs = []string{"0.0.0.0/0"}
	}

	if optionErr := s.clusterOptions(c, in); optionErr != nil {
		return nil, optionErr
	}

	c.Authority, err = authority(c.Created)
	return c, err
}

// clusterOptions applies CreateCluster's optional settings to c.
func (s *Service) clusterOptions(c *cluster, in *api.CreateClusterRequest) error {
	if n := in.KubernetesNetworkConfig; n != nil {
		if !oneOf(n.IpFamily, "ipv4") {
			return invalidParameter("TinyStack supports only the ipv4 IP family.")
		}

		if n.ServiceIpv4Cidr != nil {
			c.Network.ServiceIpv4Cidr = n.ServiceIpv4Cidr
		}
	}

	if in.Logging != nil {
		var err error
		if c.Logging, err = applyLogging(nil, in.Logging); err != nil {
			return err
		}
	}

	if a := in.AccessConfig; a != nil {
		if !oneOf(a.AuthenticationMode, configMap, apiAndConfigMap, apiMode) {
			return invalidParameter("authenticationMode must be CONFIG_MAP, API_AND_CONFIG_MAP, or API.")
		}

		c.AuthMode = common.Value(common.OrElse(a.AuthenticationMode, &c.AuthMode))
		c.Bootstrap = common.Value(common.OrElse(a.BootstrapClusterCreatorAdminPermissions, &c.Bootstrap))
	}

	if u := in.UpgradePolicy; u != nil {
		if !oneOf(u.SupportType, "STANDARD", extended) {
			return invalidParameter("supportType must be STANDARD or EXTENDED.")
		}

		c.Support = common.Value(common.OrElse(u.SupportType, &c.Support))
	}

	for _, e := range in.EncryptionConfig {
		if !slices.Equal(e.Resources, []string{"secrets"}) || common.Value(common.Value(e.Provider).KeyArn) == "" {
			return invalidParameter("Encryption configurations encrypt secrets with a KMS key ARN.")
		}
	}

	if len(in.EncryptionConfig) > 1 {
		return invalidParameter("A cluster can have one encryption configuration.")
	}

	return nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}

	return v
}

func oneOf(v *string, allowed ...string) bool {
	return v == nil || slices.Contains(allowed, *v)
}

func (s *Service) DescribeCluster(
	ctx context.Context,
	in *api.DescribeClusterRequest,
) (*api.DescribeClusterResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Cluster
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		out = c.output(scope)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeClusterResponse{Cluster: out}, nil
}

func (s *Service) ListClusters(ctx context.Context, in *api.ListClustersRequest) (*api.ListClustersResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListClustersResponse{Clusters: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var names []string
		for _, k := range tx.Keys(prefix(scope, kindCluster)) {
			names = append(names, strings.TrimPrefix(k, prefix(scope, kindCluster)))
		}

		var pageErr error
		names, out.NextToken, pageErr = page(names, func(n string) string { return n }, in.NextToken, in.MaxResults)
		out.Clusters = append(out.Clusters, names...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// clusterKinds are the kinds of records a cluster owns, deleted with it.
var clusterKinds = []string{
	kindNodegroup, kindFargate, kindAddon, "access", "pod-identity", kindIdentityProvider, "update",
}

// DeleteCluster deletes a cluster without node groups or Fargate profiles,
// with its add-ons, access entries, pod identity associations, and identity
// provider configs.
func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterRequest) (*api.DeleteClusterResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case c.DeletionProtection:
			return invalidRequest("Cluster %s has deletion protection enabled.", c.Name)
		case len(tx.Keys(prefix(scope, kindNodegroup)+c.Name+"/")) > 0:
			return inUse("Cluster has nodegroups attached")
		case len(tx.Keys(prefix(scope, kindFargate)+c.Name+"/")) > 0:
			return inUse("Cluster has fargate profiles attached")
		}

		for _, kind := range clusterKinds {
			for _, k := range tx.Keys(prefix(scope, kind) + c.Name + "/") {
				tx.Delete(k)
			}
		}

		tx.Delete(clusterKey(scope, c.Name))
		c.Status = deleting
		out = c.output(scope)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteClusterResponse{Cluster: out}, nil
}

// clusterChange is one kind of UpdateClusterConfig change.
type clusterChange struct {
	apply func(*cluster) ([]api.UpdateParam, error)
	kind  string
}

func param(kind, v string) api.UpdateParam { return api.UpdateParam{Type: new(kind), Value: new(v)} }

func jsonParam(kind string, v any) api.UpdateParam {
	data, _ := json.Marshal(v)
	return param(kind, string(data))
}

// clusterChanges returns the changes an UpdateClusterConfig request makes.
func (s *Service) clusterChanges(ctx context.Context, scope request.Scope, in *api.UpdateClusterConfigRequest) (
	[]clusterChange, error,
) {
	var changes []clusterChange
	if v := in.ResourcesVpcConfig; v != nil {
		change, err := s.vpcChange(ctx, scope, v)
		if err != nil {
			return nil, err
		}

		changes = append(changes, change)
	}

	if in.Logging != nil {
		changes = append(
			changes,
			clusterChange{kind: "LoggingUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
				enabled, err := applyLogging(c.Logging, in.Logging)
				c.Logging = enabled
				return []api.UpdateParam{jsonParam("ClusterLogging", logging(enabled))}, err
			}},
		)
	}

	if a := in.AccessConfig; a != nil {
		changes = append(
			changes,
			clusterChange{kind: "AccessConfigUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
				// Modes only move from the config map towards the API.
				order := []string{configMap, apiAndConfigMap, apiMode}
				to := slices.Index(order, common.Value(a.AuthenticationMode))
				if to < 0 || to < slices.Index(order, c.AuthMode) {
					return nil, invalidParameter("Unsupported authentication mode update from %s to %s", c.AuthMode,
						common.Value(a.AuthenticationMode))
				}

				c.AuthMode = order[to]
				return []api.UpdateParam{param("AuthenticationMode", c.AuthMode)}, nil
			}},
		)
	}

	if u := in.UpgradePolicy; u != nil {
		changes = append(
			changes,
			clusterChange{kind: "UpgradePolicyUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
				if u.SupportType == nil || !oneOf(u.SupportType, "STANDARD", extended) {
					return nil, invalidParameter("supportType must be STANDARD or EXTENDED.")
				}

				c.Support = *u.SupportType
				return []api.UpdateParam{jsonParam("UpgradePolicy", u)}, nil
			}},
		)
	}

	if z := in.ZonalShiftConfig; z != nil {
		changes = append(
			changes,
			clusterChange{kind: "ZonalShiftConfigUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
				c.ZonalShift = new(common.Value(z.Enabled))
				return []api.UpdateParam{jsonParam("ZonalShiftConfig", z)}, nil
			}},
		)
	}

	if d := in.DeletionProtection; d != nil {
		changes = append(
			changes,
			clusterChange{kind: "DeletionProtectionUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
				c.DeletionProtection = *d
				return []api.UpdateParam{param("DeletionProtection", strconv.FormatBool(*d))}, nil
			}},
		)
	}

	switch {
	case len(changes) == 0:
		return nil, invalidParameter("No changes needed for the logging config provided")
	case len(changes) > 1:
		return nil, invalidParameter("Only one type of update can be allowed.")
	}

	return changes, nil
}

// vpcChange is a change of a cluster's subnets and security groups, or of
// its endpoint access.
func (s *Service) vpcChange(ctx context.Context, scope request.Scope, v *api.VpcConfigRequest) (clusterChange, error) {
	if v.SubnetIds == nil && v.SecurityGroupIds == nil {
		return clusterChange{kind: "EndpointAccessUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
			if err := checkEndpoints(v, &c.Vpc); err != nil {
				return nil, err
			}

			c.Vpc.EndpointPublicAccess = common.OrElse(v.EndpointPublicAccess, c.Vpc.EndpointPublicAccess)
			c.Vpc.EndpointPrivateAccess = common.OrElse(v.EndpointPrivateAccess, c.Vpc.EndpointPrivateAccess)
			if v.PublicAccessCidrs != nil {
				c.Vpc.PublicAccessCidrs = v.PublicAccessCidrs
			}

			return []api.UpdateParam{
				param("EndpointPublicAccess", strconv.FormatBool(*c.Vpc.EndpointPublicAccess)),
				param("EndpointPrivateAccess", strconv.FormatBool(*c.Vpc.EndpointPrivateAccess)),
				jsonParam("PublicAccessCidrs", c.Vpc.PublicAccessCidrs),
			}, nil
		}}, nil
	}

	placed, err := s.place(ctx, scope, v.SubnetIds, v.SecurityGroupIds)
	if err != nil {
		return clusterChange{}, err
	}

	return clusterChange{kind: "VpcConfigUpdate", apply: func(c *cluster) ([]api.UpdateParam, error) {
		if placed.VPC != "" && placed.VPC != common.Value(c.Vpc.VpcId) {
			return nil, invalidParameter("Subnets and security groups must be in the cluster's VPC %s.",
				common.Value(c.Vpc.VpcId))
		}

		var params []api.UpdateParam
		if v.SubnetIds != nil {
			c.Vpc.SubnetIds = v.SubnetIds
			params = append(params, jsonParam("Subnets", v.SubnetIds))
		}

		if v.SecurityGroupIds != nil {
			c.Vpc.SecurityGroupIds = v.SecurityGroupIds
			params = append(params, jsonParam("SecurityGroups", v.SecurityGroupIds))
		}

		return params, nil
	}}, nil
}

func (s *Service) UpdateClusterConfig(
	ctx context.Context,
	in *api.UpdateClusterConfigRequest,
) (*api.UpdateClusterConfigResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	changes, err := s.clusterChanges(ctx, scope, in)
	if err != nil {
		return nil, err
	}

	var out *api.Update
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		params, applyErr := changes[0].apply(c)
		if applyErr != nil {
			return applyErr
		}

		if out, applyErr = s.record(tx, scope, c.Name, "", "", changes[0].kind, params); applyErr != nil {
			return applyErr
		}

		return storage.Write(tx, clusterKey(scope, c.Name), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateClusterConfigResponse{Update: out}, nil
}

// UpdateClusterVersion upgrades a cluster one minor version.
func (s *Service) UpdateClusterVersion(
	ctx context.Context,
	in *api.UpdateClusterVersionRequest,
) (*api.UpdateClusterVersionResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Update
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Name)
		if loadErr != nil {
			return loadErr
		}

		if s.runner != nil {
			return invalidRequest("The Kubernetes API servers TinyStack runs keep the version they started with.")
		}

		if to := versionIndex(common.Value(in.Version)); to < 0 || to != versionIndex(c.Version)+1 {
			return invalidParameter("Unsupported Kubernetes minor version update from %s to %s", c.Version,
				common.Value(in.Version))
		}

		c.Version = *in.Version
		params := []api.UpdateParam{param("Version", c.Version), param("PlatformVersion", platformVersion)}
		var recordErr error
		if out, recordErr = s.record(tx, scope, c.Name, "", "", "VersionUpdate", params); recordErr != nil {
			return recordErr
		}

		return storage.Write(tx, clusterKey(scope, c.Name), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateClusterVersionResponse{Update: out}, nil
}

func (s *Service) DescribeClusterVersions(
	_ context.Context,
	in *api.DescribeClusterVersionsRequest,
) (*api.DescribeClusterVersionsResponse, error) {
	if !oneOf(in.ClusterType, "eks") {
		return nil, invalidParameter("TinyStack supports only the eks cluster type.")
	}

	now := s.now().UTC()
	var versions []api.ClusterVersionInformation
	for _, k := range kubeVersions {
		standardEnd, extendedEnd := k.released.AddDate(0, 14, 0), k.released.AddDate(0, 26, 0)
		status := "STANDARD_SUPPORT"
		if now.After(standardEnd) {
			status = "EXTENDED_SUPPORT"
		}

		wanted := len(in.ClusterVersions) == 0 || slices.Contains(in.ClusterVersions, k.name)
		if !wanted || common.Value(in.DefaultOnly) && k.name != defaultVersion ||
			now.After(extendedEnd) && !common.Value(in.IncludeAll) {
			continue
		}

		versions = append(versions, api.ClusterVersionInformation{
			ClusterVersion:         new(k.name),
			ClusterType:            new("eks"),
			DefaultPlatformVersion: new(platformVersion),
			DefaultVersion:         new(k.name == defaultVersion),
			ReleaseDate:            common.SecondsMilli(k.released),
			EndOfStandardSupportDate: common.SecondsMilli(
				standardEnd,
			),
			EndOfExtendedSupportDate: common.SecondsMilli(extendedEnd),
			Status:                   new(status),
			VersionStatus:            new(status),
			KubernetesPatchVersion:   new(k.patch),
		})
	}

	versions, next, err := page(versions, func(v api.ClusterVersionInformation) string { return *v.ClusterVersion },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.DescribeClusterVersionsResponse{ClusterVersions: versions, NextToken: next}, nil
}

// hexID returns n random lowercase hexadecimal digits.
func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
