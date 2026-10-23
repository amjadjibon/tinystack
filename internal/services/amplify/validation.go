package amplify

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

type stringRule struct {
	pattern  *regexp.Regexp
	values   []string
	min, max int
}

var stringRules = map[string]stringRule{
	"FileName":                     {min: 0, max: 255},
	"MD5Hash":                      {min: 0, max: 32},
	"AccessToken":                  {min: 1, max: 4096},
	"AppId":                        {min: 1, max: 20, pattern: regexp.MustCompile(`^d[a-z0-9]+$`)},
	"AutoBranchCreationPattern":    {min: 1, max: 2048},
	"AutoSubDomainCreationPattern": {min: 1, max: 2048},
	"AutoSubDomainIAMRole":         {min: 0, max: 1000, pattern: regexp.MustCompile(`^$|^arn:aws:iam::\d{12}:role.+$`)},
	"BackendEnvironmentArn":        {min: 0, max: 1000},
	"BasicAuthCredentials":         {min: 0, max: 2000},
	"BranchName":                   {min: 1, max: 255},
	"BuildComputeType":             {min: 0, max: -1, values: []string{"STANDARD_8GB", "LARGE_16GB", "XLARGE_72GB"}},
	"BuildSpec":                    {min: 1, max: 25000},
	"CacheConfigType": {
		min:    0,
		max:    -1,
		values: []string{"AMPLIFY_MANAGED", "AMPLIFY_MANAGED_NO_COOKIES"},
	},
	"CertificateArn": {
		min:     0,
		max:     1000,
		pattern: regexp.MustCompile(`^arn:aws:acm:[a-z0-9-]+:\d{12}:certificate/.+$`),
	},
	"CertificateType":     {min: 0, max: -1, values: []string{"AMPLIFY_MANAGED", certificateCustom}},
	"CommitId":            {min: 0, max: 255},
	"CommitMessage":       {min: 0, max: 10000},
	"ComputeRoleArn":      {min: 0, max: 1000},
	"Condition":           {min: 0, max: 2048},
	"CustomHeaders":       {min: 0, max: 25000},
	"DeploymentArtifacts": {min: 1, max: 1000},
	"Description":         {min: 0, max: 1000},
	"DisplayName":         {min: 0, max: 255},
	"DomainName": {
		min: 0,
		max: 64,
		pattern: regexp.MustCompile(
			`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])\.?$`,
		),
	},
	"DomainPrefix":               {min: 0, max: 255},
	"EnvKey":                     {min: 0, max: 255},
	"EnvValue":                   {min: 0, max: 5500},
	"EnvironmentName":            {min: 1, max: 255},
	"Framework":                  {min: 0, max: 255},
	"JobId":                      {min: 0, max: 255, pattern: regexp.MustCompile(`^\d+$`)},
	"JobReason":                  {min: 0, max: 255},
	"JobType":                    {min: 0, max: 10, values: []string{"RELEASE", jobRetry, "MANUAL", "WEB_HOOK"}},
	"Name":                       {min: 1, max: 255},
	"NextToken":                  {min: 0, max: 2000},
	"OauthToken":                 {min: 0, max: 4096},
	"Platform":                   {min: 0, max: -1, values: []string{"WEB", "WEB_DYNAMIC", "WEB_COMPUTE"}},
	"PullRequestEnvironmentName": {min: 0, max: 20},
	"Repository":                 {min: 0, max: 1000},
	"ResourceArn":                {min: 0, max: 2048, pattern: regexp.MustCompile(`^arn:aws:amplify:`)},
	"ServiceRoleArn":             {min: 0, max: 1000},
	"Source":                     {min: 1, max: 2048},
	"SourceUrl":                  {min: 0, max: 3000, pattern: regexp.MustCompile(`^(s3|https|http)://`)},
	"SourceUrlType":              {min: 0, max: -1, values: []string{"ZIP", "BUCKET_PREFIX"}},
	"StackArn": {
		min:     20,
		max:     2048,
		pattern: regexp.MustCompile(`^arn:aws:cloudformation:[a-z0-9-]+:\d{12}:stack/.+/.+$`),
	},
	"StackName": {min: 1, max: 255},
	"Stage": {
		min:    0,
		max:    -1,
		values: []string{"PRODUCTION", "BETA", "DEVELOPMENT", "EXPERIMENTAL", "PULL_REQUEST"},
	},
	"Status":    {min: 3, max: 7, pattern: regexp.MustCompile(`^.{3,7}$`)},
	"TTL":       {min: 0, max: 32, pattern: regexp.MustCompile(`^\d*$`)},
	"TagKey":    {min: 1, max: 128, pattern: regexp.MustCompile(`^[a-zA-Z+\-=._:/]+$`)},
	"TagValue":  {min: 0, max: 256, pattern: regexp.MustCompile(`^([\p{L}\p{Z}\p{N}_.:/=+\-@]*)$`)},
	"Target":    {min: 1, max: 2048},
	"WebhookId": {min: 0, max: 255},
}

func checkString(field string, value *string, shape string) error {
	if value == nil {
		return nil
	}

	r := stringRules[shape]
	n := utf8.RuneCountInString(*value)
	if n < r.min || r.max >= 0 && n > r.max || r.pattern != nil && !r.pattern.MatchString(*value) ||
		len(r.values) > 0 && !slices.Contains(r.values, *value) ||
		shape == "TagKey" && strings.HasPrefix(*value, "aws:") {
		return bad("%s violates the %s model constraint.", field, shape)
	}

	return nil
}

func validateCustomRule(in *api.CustomRule) error {
	if in.Source == nil {
		return bad("source is required.")
	}

	if err := checkString("source", in.Source, "Source"); err != nil {
		return err
	}

	if in.Target == nil {
		return bad("target is required.")
	}

	if err := checkString("target", in.Target, "Target"); err != nil {
		return err
	}

	if err := checkString("status", in.Status, "Status"); err != nil {
		return err
	}

	if err := checkString("condition", in.Condition, "Condition"); err != nil {
		return err
	}

	return nil
}

func validateAutoBranchCreationConfig(in *api.AutoBranchCreationConfig) error {
	if err := checkString("stage", in.Stage, "Stage"); err != nil {
		return err
	}

	if err := checkString("framework", in.Framework, "Framework"); err != nil {
		return err
	}

	for key, value := range in.EnvironmentVariables {
		if err := checkString("environmentVariables key", new(key), "EnvKey"); err != nil {
			return err
		}

		if err := checkString("environmentVariables value", new(value), "EnvValue"); err != nil {
			return err
		}
	}

	if err := checkString("basicAuthCredentials", in.BasicAuthCredentials, "BasicAuthCredentials"); err != nil {
		return err
	}

	if err := checkString("buildSpec", in.BuildSpec, "BuildSpec"); err != nil {
		return err
	}

	if err := checkString(
		"pullRequestEnvironmentName",
		in.PullRequestEnvironmentName,
		"PullRequestEnvironmentName",
	); err != nil {
		return err
	}

	return nil
}

func validateJobConfig(in *api.JobConfig) error {
	if in.BuildComputeType == nil {
		return bad("buildComputeType is required.")
	}

	if err := checkString("buildComputeType", in.BuildComputeType, "BuildComputeType"); err != nil {
		return err
	}

	return nil
}

func validateCacheConfig(in *api.CacheConfig) error {
	if in.Type == nil {
		return bad("type is required.")
	}

	if err := checkString("type", in.Type, "CacheConfigType"); err != nil {
		return err
	}

	return nil
}

func validateCreateAppRequest(in *api.CreateAppRequest) error {
	if in.Name == nil {
		return bad("name is required.")
	}

	if err := checkString("name", in.Name, "Name"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	if err := checkString("repository", in.Repository, "Repository"); err != nil {
		return err
	}

	if err := checkString("platform", in.Platform, "Platform"); err != nil {
		return err
	}

	if err := checkString("computeRoleArn", in.ComputeRoleArn, "ComputeRoleArn"); err != nil {
		return err
	}

	if err := checkString("iamServiceRoleArn", in.IamServiceRoleArn, "ServiceRoleArn"); err != nil {
		return err
	}

	if err := checkString("oauthToken", in.OauthToken, "OauthToken"); err != nil {
		return err
	}

	if err := checkString("accessToken", in.AccessToken, "AccessToken"); err != nil {
		return err
	}

	for key, value := range in.EnvironmentVariables {
		if err := checkString("environmentVariables key", new(key), "EnvKey"); err != nil {
			return err
		}

		if err := checkString("environmentVariables value", new(value), "EnvValue"); err != nil {
			return err
		}
	}

	if err := checkString("basicAuthCredentials", in.BasicAuthCredentials, "BasicAuthCredentials"); err != nil {
		return err
	}

	for _, item := range in.CustomRules {
		if err := validateCustomRule(new(item)); err != nil {
			return err
		}
	}

	if len(in.Tags) > 50 {
		return bad("tags has invalid length.")
	}

	for key, value := range in.Tags {
		if err := checkString("tags key", new(key), "TagKey"); err != nil {
			return err
		}

		if err := checkString("tags value", new(value), "TagValue"); err != nil {
			return err
		}
	}

	if err := checkString("buildSpec", in.BuildSpec, "BuildSpec"); err != nil {
		return err
	}

	if err := checkString("customHeaders", in.CustomHeaders, "CustomHeaders"); err != nil {
		return err
	}

	for _, item := range in.AutoBranchCreationPatterns {
		if err := checkString("autoBranchCreationPatterns", new(item), "AutoBranchCreationPattern"); err != nil {
			return err
		}
	}

	if in.AutoBranchCreationConfig != nil {
		if err := validateAutoBranchCreationConfig(in.AutoBranchCreationConfig); err != nil {
			return err
		}
	}

	if in.JobConfig != nil {
		if err := validateJobConfig(in.JobConfig); err != nil {
			return err
		}
	}

	if in.CacheConfig != nil {
		if err := validateCacheConfig(in.CacheConfig); err != nil {
			return err
		}
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return bad("%s", err)
	}

	return nil
}

func validateCreateBackendEnvironmentRequest(in *api.CreateBackendEnvironmentRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.EnvironmentName == nil {
		return bad("environmentName is required.")
	}

	if err := checkString("environmentName", in.EnvironmentName, "EnvironmentName"); err != nil {
		return err
	}

	if err := checkString("stackName", in.StackName, "StackName"); err != nil {
		return err
	}

	if err := checkString("deploymentArtifacts", in.DeploymentArtifacts, "DeploymentArtifacts"); err != nil {
		return err
	}

	return nil
}

func validateBackend(in *api.Backend) error {
	if err := checkString("stackArn", in.StackArn, "StackArn"); err != nil {
		return err
	}

	return nil
}

func validateCreateBranchRequest(in *api.CreateBranchRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	if err := checkString("stage", in.Stage, "Stage"); err != nil {
		return err
	}

	if err := checkString("framework", in.Framework, "Framework"); err != nil {
		return err
	}

	for key, value := range in.EnvironmentVariables {
		if err := checkString("environmentVariables key", new(key), "EnvKey"); err != nil {
			return err
		}

		if err := checkString("environmentVariables value", new(value), "EnvValue"); err != nil {
			return err
		}
	}

	if err := checkString("basicAuthCredentials", in.BasicAuthCredentials, "BasicAuthCredentials"); err != nil {
		return err
	}

	if len(in.Tags) > 50 {
		return bad("tags has invalid length.")
	}

	for key, value := range in.Tags {
		if err := checkString("tags key", new(key), "TagKey"); err != nil {
			return err
		}

		if err := checkString("tags value", new(value), "TagValue"); err != nil {
			return err
		}
	}

	if err := checkString("buildSpec", in.BuildSpec, "BuildSpec"); err != nil {
		return err
	}

	if err := checkString("ttl", in.Ttl, "TTL"); err != nil {
		return err
	}

	if err := checkString("displayName", in.DisplayName, "DisplayName"); err != nil {
		return err
	}

	if err := checkString(
		"pullRequestEnvironmentName",
		in.PullRequestEnvironmentName,
		"PullRequestEnvironmentName",
	); err != nil {
		return err
	}

	if err := checkString("backendEnvironmentArn", in.BackendEnvironmentArn, "BackendEnvironmentArn"); err != nil {
		return err
	}

	if in.Backend != nil {
		if err := validateBackend(in.Backend); err != nil {
			return err
		}
	}

	if err := checkString("computeRoleArn", in.ComputeRoleArn, "ComputeRoleArn"); err != nil {
		return err
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return bad("%s", err)
	}

	return nil
}

func validateCreateDeploymentRequest(in *api.CreateDeploymentRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	for name, hash := range in.FileMap {
		if err := checkString("fileMap key", new(name), "FileName"); err != nil {
			return err
		}

		if err := checkString("fileMap value", new(hash), "MD5Hash"); err != nil {
			return err
		}
	}

	return nil
}

func validateSubDomainSetting(in *api.SubDomainSetting) error {
	if in.Prefix == nil {
		return bad("prefix is required.")
	}

	if err := checkString("prefix", in.Prefix, "DomainPrefix"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	return nil
}

func validateCertificateSettings(in *api.CertificateSettings) error {
	if in.Type == nil {
		return bad("type is required.")
	}

	if err := checkString("type", in.Type, "CertificateType"); err != nil {
		return err
	}

	if err := checkString("customCertificateArn", in.CustomCertificateArn, "CertificateArn"); err != nil {
		return err
	}

	return nil
}

func validateCreateDomainAssociationRequest(in *api.CreateDomainAssociationRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.DomainName == nil {
		return bad("domainName is required.")
	}

	if err := checkString("domainName", in.DomainName, "DomainName"); err != nil {
		return err
	}

	if in.SubDomainSettings == nil {
		return bad("subDomainSettings is required.")
	}

	if len(in.SubDomainSettings) > 500 {
		return bad("subDomainSettings has invalid length.")
	}

	for _, item := range in.SubDomainSettings {
		if err := validateSubDomainSetting(new(item)); err != nil {
			return err
		}
	}

	for _, item := range in.AutoSubDomainCreationPatterns {
		if err := checkString("autoSubDomainCreationPatterns", new(item), "AutoSubDomainCreationPattern"); err != nil {
			return err
		}
	}

	if err := checkString("autoSubDomainIAMRole", in.AutoSubDomainIAMRole, "AutoSubDomainIAMRole"); err != nil {
		return err
	}

	if in.CertificateSettings != nil {
		if err := validateCertificateSettings(in.CertificateSettings); err != nil {
			return err
		}
	}

	if in.CertificateSettings != nil && common.Value(in.CertificateSettings.Type) == certificateCustom &&
		in.CertificateSettings.CustomCertificateArn == nil {
		return bad("customCertificateArn is required for CUSTOM certificates.")
	}

	return nil
}

func validateCreateWebhookRequest(in *api.CreateWebhookRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	return nil
}

func validateDeleteAppRequest(in *api.DeleteAppRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	return nil
}

func validateDeleteBackendEnvironmentRequest(in *api.DeleteBackendEnvironmentRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.EnvironmentName == nil {
		return bad("environmentName is required.")
	}

	if err := checkString("environmentName", in.EnvironmentName, "EnvironmentName"); err != nil {
		return err
	}

	return nil
}

func validateDeleteBranchRequest(in *api.DeleteBranchRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	return nil
}

func validateDeleteDomainAssociationRequest(in *api.DeleteDomainAssociationRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.DomainName == nil {
		return bad("domainName is required.")
	}

	if err := checkString("domainName", in.DomainName, "DomainName"); err != nil {
		return err
	}

	return nil
}

func validateDeleteJobRequest(in *api.DeleteJobRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if in.JobId == nil {
		return bad("jobId is required.")
	}

	if err := checkString("jobId", in.JobId, "JobId"); err != nil {
		return err
	}

	return nil
}

func validateDeleteWebhookRequest(in *api.DeleteWebhookRequest) error {
	if in.WebhookId == nil {
		return bad("webhookId is required.")
	}

	if err := checkString("webhookId", in.WebhookId, "WebhookId"); err != nil {
		return err
	}

	return nil
}

func validateGetAppRequest(in *api.GetAppRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	return nil
}

func validateGetBackendEnvironmentRequest(in *api.GetBackendEnvironmentRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.EnvironmentName == nil {
		return bad("environmentName is required.")
	}

	if err := checkString("environmentName", in.EnvironmentName, "EnvironmentName"); err != nil {
		return err
	}

	return nil
}

func validateGetBranchRequest(in *api.GetBranchRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	return nil
}

func validateGetDomainAssociationRequest(in *api.GetDomainAssociationRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.DomainName == nil {
		return bad("domainName is required.")
	}

	if err := checkString("domainName", in.DomainName, "DomainName"); err != nil {
		return err
	}

	return nil
}

func validateGetJobRequest(in *api.GetJobRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if in.JobId == nil {
		return bad("jobId is required.")
	}

	if err := checkString("jobId", in.JobId, "JobId"); err != nil {
		return err
	}

	return nil
}

func validateGetWebhookRequest(in *api.GetWebhookRequest) error {
	if in.WebhookId == nil {
		return bad("webhookId is required.")
	}

	if err := checkString("webhookId", in.WebhookId, "WebhookId"); err != nil {
		return err
	}

	return nil
}

func validateListAppsRequest(in *api.ListAppsRequest) error {
	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 100) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateListBackendEnvironmentsRequest(in *api.ListBackendEnvironmentsRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if err := checkString("environmentName", in.EnvironmentName, "EnvironmentName"); err != nil {
		return err
	}

	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 50) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateListBranchesRequest(in *api.ListBranchesRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 50) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateListDomainAssociationsRequest(in *api.ListDomainAssociationsRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 50) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateListJobsRequest(in *api.ListJobsRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 50) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateListTagsForResourceRequest(in *api.ListTagsForResourceRequest) error {
	if in.ResourceArn == nil {
		return bad("resourceArn is required.")
	}

	if err := checkString("resourceArn", in.ResourceArn, "ResourceArn"); err != nil {
		return err
	}

	return nil
}

func validateListWebhooksRequest(in *api.ListWebhooksRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if err := checkString("nextToken", in.NextToken, "NextToken"); err != nil {
		return err
	}

	if in.MaxResults != nil && (*in.MaxResults < 0 || *in.MaxResults > 50) {
		return bad("maxResults is out of range.")
	}

	return nil
}

func validateStartDeploymentRequest(in *api.StartDeploymentRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("jobId", in.JobId, "JobId"); err != nil {
		return err
	}

	if err := checkString("sourceUrl", in.SourceUrl, "SourceUrl"); err != nil {
		return err
	}

	if err := checkString("sourceUrlType", in.SourceUrlType, "SourceUrlType"); err != nil {
		return err
	}

	return nil
}

func validateStartJobRequest(in *api.StartJobRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("jobId", in.JobId, "JobId"); err != nil {
		return err
	}

	if in.JobType == nil {
		return bad("jobType is required.")
	}

	if err := checkString("jobType", in.JobType, "JobType"); err != nil {
		return err
	}

	if err := checkString("jobReason", in.JobReason, "JobReason"); err != nil {
		return err
	}

	if err := checkString("commitId", in.CommitId, "CommitId"); err != nil {
		return err
	}

	if err := checkString("commitMessage", in.CommitMessage, "CommitMessage"); err != nil {
		return err
	}

	return nil
}

func validateStopJobRequest(in *api.StopJobRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if in.JobId == nil {
		return bad("jobId is required.")
	}

	if err := checkString("jobId", in.JobId, "JobId"); err != nil {
		return err
	}

	return nil
}

func validateTagResourceRequest(in *api.TagResourceRequest) error {
	if in.ResourceArn == nil {
		return bad("resourceArn is required.")
	}

	if err := checkString("resourceArn", in.ResourceArn, "ResourceArn"); err != nil {
		return err
	}

	if in.Tags == nil {
		return bad("tags is required.")
	}

	if len(in.Tags) > 50 {
		return bad("tags has invalid length.")
	}

	for key, value := range in.Tags {
		if err := checkString("tags key", new(key), "TagKey"); err != nil {
			return err
		}

		if err := checkString("tags value", new(value), "TagValue"); err != nil {
			return err
		}
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return bad("%s", err)
	}

	return nil
}

func validateUntagResourceRequest(in *api.UntagResourceRequest) error {
	if in.ResourceArn == nil {
		return bad("resourceArn is required.")
	}

	if err := checkString("resourceArn", in.ResourceArn, "ResourceArn"); err != nil {
		return err
	}

	if in.TagKeys == nil {
		return bad("tagKeys is required.")
	}

	if in.TagKeys != nil && (len(in.TagKeys) < 1 || len(in.TagKeys) > 50) {
		return bad("tagKeys has invalid length.")
	}

	for _, item := range in.TagKeys {
		if err := checkString("tagKeys", new(item), "TagKey"); err != nil {
			return err
		}
	}

	return nil
}

func validateUpdateAppRequest(in *api.UpdateAppRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if err := checkString("name", in.Name, "Name"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	if err := checkString("platform", in.Platform, "Platform"); err != nil {
		return err
	}

	if err := checkString("computeRoleArn", in.ComputeRoleArn, "ComputeRoleArn"); err != nil {
		return err
	}

	if err := checkString("iamServiceRoleArn", in.IamServiceRoleArn, "ServiceRoleArn"); err != nil {
		return err
	}

	for key, value := range in.EnvironmentVariables {
		if err := checkString("environmentVariables key", new(key), "EnvKey"); err != nil {
			return err
		}

		if err := checkString("environmentVariables value", new(value), "EnvValue"); err != nil {
			return err
		}
	}

	if err := checkString("basicAuthCredentials", in.BasicAuthCredentials, "BasicAuthCredentials"); err != nil {
		return err
	}

	for _, item := range in.CustomRules {
		if err := validateCustomRule(new(item)); err != nil {
			return err
		}
	}

	if err := checkString("buildSpec", in.BuildSpec, "BuildSpec"); err != nil {
		return err
	}

	if err := checkString("customHeaders", in.CustomHeaders, "CustomHeaders"); err != nil {
		return err
	}

	for _, item := range in.AutoBranchCreationPatterns {
		if err := checkString("autoBranchCreationPatterns", new(item), "AutoBranchCreationPattern"); err != nil {
			return err
		}
	}

	if in.AutoBranchCreationConfig != nil {
		if err := validateAutoBranchCreationConfig(in.AutoBranchCreationConfig); err != nil {
			return err
		}
	}

	if err := checkString("repository", in.Repository, "Repository"); err != nil {
		return err
	}

	if err := checkString("oauthToken", in.OauthToken, "OauthToken"); err != nil {
		return err
	}

	if err := checkString("accessToken", in.AccessToken, "AccessToken"); err != nil {
		return err
	}

	if in.JobConfig != nil {
		if err := validateJobConfig(in.JobConfig); err != nil {
			return err
		}
	}

	if in.CacheConfig != nil {
		if err := validateCacheConfig(in.CacheConfig); err != nil {
			return err
		}
	}

	return nil
}

func validateUpdateBranchRequest(in *api.UpdateBranchRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.BranchName == nil {
		return bad("branchName is required.")
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	if err := checkString("framework", in.Framework, "Framework"); err != nil {
		return err
	}

	if err := checkString("stage", in.Stage, "Stage"); err != nil {
		return err
	}

	for key, value := range in.EnvironmentVariables {
		if err := checkString("environmentVariables key", new(key), "EnvKey"); err != nil {
			return err
		}

		if err := checkString("environmentVariables value", new(value), "EnvValue"); err != nil {
			return err
		}
	}

	if err := checkString("basicAuthCredentials", in.BasicAuthCredentials, "BasicAuthCredentials"); err != nil {
		return err
	}

	if err := checkString("buildSpec", in.BuildSpec, "BuildSpec"); err != nil {
		return err
	}

	if err := checkString("ttl", in.Ttl, "TTL"); err != nil {
		return err
	}

	if err := checkString("displayName", in.DisplayName, "DisplayName"); err != nil {
		return err
	}

	if err := checkString(
		"pullRequestEnvironmentName",
		in.PullRequestEnvironmentName,
		"PullRequestEnvironmentName",
	); err != nil {
		return err
	}

	if err := checkString("backendEnvironmentArn", in.BackendEnvironmentArn, "BackendEnvironmentArn"); err != nil {
		return err
	}

	if in.Backend != nil {
		if err := validateBackend(in.Backend); err != nil {
			return err
		}
	}

	if err := checkString("computeRoleArn", in.ComputeRoleArn, "ComputeRoleArn"); err != nil {
		return err
	}

	return nil
}

func validateUpdateDomainAssociationRequest(in *api.UpdateDomainAssociationRequest) error {
	if in.AppId == nil {
		return bad("appId is required.")
	}

	if err := checkString("appId", in.AppId, "AppId"); err != nil {
		return err
	}

	if in.DomainName == nil {
		return bad("domainName is required.")
	}

	if err := checkString("domainName", in.DomainName, "DomainName"); err != nil {
		return err
	}

	if len(in.SubDomainSettings) > 500 {
		return bad("subDomainSettings has invalid length.")
	}

	for _, item := range in.SubDomainSettings {
		if err := validateSubDomainSetting(new(item)); err != nil {
			return err
		}
	}

	for _, item := range in.AutoSubDomainCreationPatterns {
		if err := checkString("autoSubDomainCreationPatterns", new(item), "AutoSubDomainCreationPattern"); err != nil {
			return err
		}
	}

	if err := checkString("autoSubDomainIAMRole", in.AutoSubDomainIAMRole, "AutoSubDomainIAMRole"); err != nil {
		return err
	}

	if in.CertificateSettings != nil {
		if err := validateCertificateSettings(in.CertificateSettings); err != nil {
			return err
		}
	}

	if in.CertificateSettings != nil && common.Value(in.CertificateSettings.Type) == certificateCustom &&
		in.CertificateSettings.CustomCertificateArn == nil {
		return bad("customCertificateArn is required for CUSTOM certificates.")
	}

	return nil
}

func validateUpdateWebhookRequest(in *api.UpdateWebhookRequest) error {
	if in.WebhookId == nil {
		return bad("webhookId is required.")
	}

	if err := checkString("webhookId", in.WebhookId, "WebhookId"); err != nil {
		return err
	}

	if err := checkString("branchName", in.BranchName, "BranchName"); err != nil {
		return err
	}

	if err := checkString("description", in.Description, "Description"); err != nil {
		return err
	}

	return nil
}
