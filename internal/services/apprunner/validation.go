package apprunner

import (
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
)

// Patterns follow the pinned Smithy model. The ARN suffix maximum is bounded by
// the full ARN length check because RE2 limits counted repetitions to 1000.
var (
	patternAutoScalingConfigurationName = regexp.MustCompile(
		`^(?:[A-Za-z0-9][A-Za-z0-9\-_]{3,31})$`,
	)
	patternTagValue = regexp.MustCompile(
		`^(?:.*)$`,
	)
	patternConnectionName = regexp.MustCompile(
		`^(?:[A-Za-z0-9][A-Za-z0-9\-_]{3,31})$`,
	)
	patternObservabilityConfigurationName = regexp.MustCompile(
		`^(?:[A-Za-z0-9][A-Za-z0-9\-_]{3,31})$`,
	)
	patternServiceName = regexp.MustCompile(
		`^(?:[A-Za-z0-9][A-Za-z0-9-_]{3,39})$`,
	)
	patternString = regexp.MustCompile(
		`^(?:.*)$`,
	)
	patternBuildCommand = regexp.MustCompile(
		`^(?:[^\x0a\x0d]+)$`,
	)
	patternStartCommand = regexp.MustCompile(
		`^(?:[^\x0a\x0d]+)$`,
	)
	patternRuntimeEnvironmentVariablesKey = regexp.MustCompile(
		`^(?:.*)$`,
	)
	patternRuntimeEnvironmentVariablesValue = regexp.MustCompile(
		`^(?:.*)$`,
	)
	patternSourceDirectory = regexp.MustCompile(
		`^(?:[^\x00]+)$`,
	)
	patternImageIdentifier = regexp.MustCompile(
		//nolint:gocritic // Preserve the pinned Smithy pattern, including its unescaped host dots.
		`^(?:([0-9]{12}.dkr.ecr.[a-z\-]+-[0-9]{1}.amazonaws.com\/((?:[a-z0-9]+(?:[._-][a-z0-9]+)*\/)*[a` +
			`-z0-9]+(?:[._-][a-z0-9]+)*)(:([\w+\-=.:\/@])+|@([\w\:]+))?)|(public\.ecr\.aws\/.+\/((?:[a-z0-` +
			`9]+(?:[._-][a-z0-9]+)*\/)*[a-z0-9]+(?:[._-][a-z0-9]+)*)(:([\w+\-=.:\/@])+|@([\w\:]+))?))$`,
	)
	patternAppRunnerResourceArn = regexp.MustCompile(
		`^(?:arn:aws(-[\w]+)*:[a-z0-9-\\.]{0,63}:[a-z0-9-\\.]{0,63}:[0-9]{12}:(\w|\/|-)+)$`,
	)
	patternRoleArn = regexp.MustCompile(
		`^(?:arn:(aws|aws-us-gov|aws-cn|aws-iso|aws-iso-b):iam::[0-9]{12}:(role|role\/service-role)\/[\` +
			`w+=,.@\-/]{1,1000})$`,
	)
	patternCpu = regexp.MustCompile(
		`^(?:256|512|1024|2048|4096|(0.25|0.5|1|2|4) vCPU)$`,
	)
	patternMemory = regexp.MustCompile(
		`^(?:512|1024|2048|3072|4096|6144|8192|10240|12288|(0.5|1|2|3|4|6|8|10|12) GB)$`,
	)
	patternKmsKeyArn = regexp.MustCompile(
		`^(?:arn:aws(-[\w]+)*:kms:[a-z\-]+-[0-9]{1}:[0-9]{12}:key\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-` +
			`[0-9a-f]{4}-[0-9a-f]{12})$`,
	)
	patternVpcConnectorName = regexp.MustCompile(
		`^(?:[A-Za-z0-9][A-Za-z0-9\-_]{3,39})$`,
	)
	patternNextToken = regexp.MustCompile(
		`^(?:.*)$`,
	)
)

func validateAutoScalingConfigurationName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 4 || utf8.RuneCountInString(*v) > 32 ||
		!patternAutoScalingConfigurationName.MatchString(*v) {
		return invalid("Invalid AutoScalingConfigurationName")
	}

	return nil
}

func validateASConfigMaxConcurrency(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 200 {
		return invalid("Invalid ASConfigMaxConcurrency")
	}

	return nil
}

func validateASConfigMinSize(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 25 {
		return invalid("Invalid ASConfigMinSize")
	}

	return nil
}

func validateASConfigMaxSize(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 {
		return invalid("Invalid ASConfigMaxSize")
	}

	return nil
}

func validateTagKey(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 128 || strings.HasPrefix(*v, "aws:") {
		return invalid("Invalid TagKey")
	}

	return nil
}

func validateTagValue(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 0 || utf8.RuneCountInString(*v) > 256 || !patternTagValue.MatchString(*v) {
		return invalid("Invalid TagValue")
	}

	return nil
}

func validateTag(in *api.Tag) error {
	if in == nil {
		return invalid("Tag is required")
	}

	if err := validateTagKey(in.Key); err != nil {
		return err
	}

	if err := validateTagValue(in.Value); err != nil {
		return err
	}

	return nil
}

func validateTagList(v []api.Tag) error {
	if v == nil {
		return nil
	}

	for _, item := range v {
		if err := validateTag(&item); err != nil {
			return err
		}
	}

	return nil
}

func validateCreateAutoScalingConfigurationRequest(in *api.CreateAutoScalingConfigurationRequest) error {
	if in == nil {
		return invalid("CreateAutoScalingConfigurationRequest is required")
	}

	if in.AutoScalingConfigurationName == nil {
		return invalid("AutoScalingConfigurationName is required")
	}

	if err := validateAutoScalingConfigurationName(in.AutoScalingConfigurationName); err != nil {
		return err
	}

	if err := validateASConfigMaxConcurrency(in.MaxConcurrency); err != nil {
		return err
	}

	if err := validateASConfigMinSize(in.MinSize); err != nil {
		return err
	}

	if err := validateASConfigMaxSize(in.MaxSize); err != nil {
		return err
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	return nil
}

func validateConnectionName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 4 || utf8.RuneCountInString(*v) > 32 || !patternConnectionName.MatchString(*v) {
		return invalid("Invalid ConnectionName")
	}

	return nil
}

func validateProviderType(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "GITHUB" && *v != "BITBUCKET" {
		return invalid("Invalid ProviderType")
	}

	return nil
}

func validateCreateConnectionRequest(in *api.CreateConnectionRequest) error {
	if in == nil {
		return invalid("CreateConnectionRequest is required")
	}

	if in.ConnectionName == nil {
		return invalid("ConnectionName is required")
	}

	if err := validateConnectionName(in.ConnectionName); err != nil {
		return err
	}

	if in.ProviderType == nil {
		return invalid("ProviderType is required")
	}

	if err := validateProviderType(in.ProviderType); err != nil {
		return err
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	return nil
}

func validateObservabilityConfigurationName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 4 || utf8.RuneCountInString(*v) > 32 ||
		!patternObservabilityConfigurationName.MatchString(*v) {
		return invalid("Invalid ObservabilityConfigurationName")
	}

	return nil
}

func validateTracingVendor(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "AWSXRAY" {
		return invalid("Invalid TracingVendor")
	}

	return nil
}

func validateTraceConfiguration(in *api.TraceConfiguration) error {
	if in == nil {
		return invalid("TraceConfiguration is required")
	}

	if in.Vendor == nil {
		return invalid("Vendor is required")
	}

	if err := validateTracingVendor(in.Vendor); err != nil {
		return err
	}

	return nil
}

func validateCreateObservabilityConfigurationRequest(in *api.CreateObservabilityConfigurationRequest) error {
	if in == nil {
		return invalid("CreateObservabilityConfigurationRequest is required")
	}

	if in.ObservabilityConfigurationName == nil {
		return invalid("ObservabilityConfigurationName is required")
	}

	if err := validateObservabilityConfigurationName(in.ObservabilityConfigurationName); err != nil {
		return err
	}

	if in.TraceConfiguration != nil {
		if err := validateTraceConfiguration(in.TraceConfiguration); err != nil {
			return err
		}
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	return nil
}

func validateServiceName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 4 || utf8.RuneCountInString(*v) > 40 || !patternServiceName.MatchString(*v) {
		return invalid("Invalid ServiceName")
	}

	return nil
}

func validateString(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 0 || utf8.RuneCountInString(*v) > 51200 || !patternString.MatchString(*v) {
		return invalid("Invalid String")
	}

	return nil
}

func validateSourceCodeVersionType(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "BRANCH" {
		return invalid("Invalid SourceCodeVersionType")
	}

	return nil
}

func validateSourceCodeVersion(in *api.SourceCodeVersion) error {
	if in == nil {
		return invalid("SourceCodeVersion is required")
	}

	if in.Type == nil {
		return invalid("Type is required")
	}

	if err := validateSourceCodeVersionType(in.Type); err != nil {
		return err
	}

	if in.Value == nil {
		return invalid("Value is required")
	}

	if err := validateString(in.Value); err != nil {
		return err
	}

	return nil
}

func validateConfigurationSource(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "REPOSITORY" && *v != "API" {
		return invalid("Invalid ConfigurationSource")
	}

	return nil
}

func validateRuntime(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "PYTHON_3" && *v != "NODEJS_12" && *v != "NODEJS_14" && *v != "CORRETTO_8" && *v != "CORRETTO_11" &&
		*v != "NODEJS_16" &&
		*v != "GO_1" &&
		*v != "DOTNET_6" &&
		*v != "PHP_81" &&
		*v != "RUBY_31" &&
		*v != "PYTHON_311" &&
		*v != "NODEJS_18" &&
		*v != "NODEJS_22" {
		return invalid("Invalid Runtime")
	}

	return nil
}

func validateBuildCommand(v *string) error {
	if v == nil {
		return nil
	}

	if !patternBuildCommand.MatchString(*v) {
		return invalid("Invalid BuildCommand")
	}

	return nil
}

func validateStartCommand(v *string) error {
	if v == nil {
		return nil
	}

	if !patternStartCommand.MatchString(*v) {
		return invalid("Invalid StartCommand")
	}

	return nil
}

func validateRuntimeEnvironmentVariablesKey(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 51200 ||
		!patternRuntimeEnvironmentVariablesKey.MatchString(*v) {
		return invalid("Invalid RuntimeEnvironmentVariablesKey")
	}

	return nil
}

func validateRuntimeEnvironmentVariablesValue(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 0 || utf8.RuneCountInString(*v) > 51200 ||
		!patternRuntimeEnvironmentVariablesValue.MatchString(*v) {
		return invalid("Invalid RuntimeEnvironmentVariablesValue")
	}

	return nil
}

func validateRuntimeEnvironmentVariables(v map[string]string) error {
	if v == nil {
		return nil
	}

	for key, value := range v {
		if err := validateRuntimeEnvironmentVariablesKey(new(key)); err != nil {
			return err
		}

		if err := validateRuntimeEnvironmentVariablesValue(new(value)); err != nil {
			return err
		}
	}

	return nil
}

func validateRuntimeEnvironmentSecretsName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 2048 {
		return invalid("Invalid RuntimeEnvironmentSecretsName")
	}

	return nil
}

func validateRuntimeEnvironmentSecretsValue(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 2048 {
		return invalid("Invalid RuntimeEnvironmentSecretsValue")
	}

	return nil
}

func validateRuntimeEnvironmentSecrets(v map[string]string) error {
	if v == nil {
		return nil
	}

	for key, value := range v {
		if err := validateRuntimeEnvironmentSecretsName(new(key)); err != nil {
			return err
		}

		if err := validateRuntimeEnvironmentSecretsValue(new(value)); err != nil {
			return err
		}
	}

	return nil
}

func validateCodeConfigurationValues(in *api.CodeConfigurationValues) error {
	if in == nil {
		return invalid("CodeConfigurationValues is required")
	}

	if in.Runtime == nil {
		return invalid("Runtime is required")
	}

	if err := validateRuntime(in.Runtime); err != nil {
		return err
	}

	if err := validateBuildCommand(in.BuildCommand); err != nil {
		return err
	}

	if err := validateStartCommand(in.StartCommand); err != nil {
		return err
	}

	if err := validateString(in.Port); err != nil {
		return err
	}

	if err := validateRuntimeEnvironmentVariables(in.RuntimeEnvironmentVariables); err != nil {
		return err
	}

	if err := validateRuntimeEnvironmentSecrets(in.RuntimeEnvironmentSecrets); err != nil {
		return err
	}

	return nil
}

func validateCodeConfiguration(in *api.CodeConfiguration) error {
	if in == nil {
		return invalid("CodeConfiguration is required")
	}

	if in.ConfigurationSource == nil {
		return invalid("ConfigurationSource is required")
	}

	if err := validateConfigurationSource(in.ConfigurationSource); err != nil {
		return err
	}

	if in.CodeConfigurationValues != nil {
		if err := validateCodeConfigurationValues(in.CodeConfigurationValues); err != nil {
			return err
		}
	}

	return nil
}

func validateSourceDirectory(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 4096 || !patternSourceDirectory.MatchString(*v) {
		return invalid("Invalid SourceDirectory")
	}

	return nil
}

func validateCodeRepository(in *api.CodeRepository) error {
	if in == nil {
		return invalid("CodeRepository is required")
	}

	if in.RepositoryUrl == nil {
		return invalid("RepositoryUrl is required")
	}

	if err := validateString(in.RepositoryUrl); err != nil {
		return err
	}

	if in.SourceCodeVersion == nil {
		return invalid("SourceCodeVersion is required")
	}

	if in.SourceCodeVersion != nil {
		if err := validateSourceCodeVersion(in.SourceCodeVersion); err != nil {
			return err
		}
	}

	if in.CodeConfiguration != nil {
		if err := validateCodeConfiguration(in.CodeConfiguration); err != nil {
			return err
		}
	}

	if err := validateSourceDirectory(in.SourceDirectory); err != nil {
		return err
	}

	return nil
}

func validateImageIdentifier(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 1024 || !patternImageIdentifier.MatchString(*v) {
		return invalid("Invalid ImageIdentifier")
	}

	return nil
}

func validateImageConfiguration(in *api.ImageConfiguration) error {
	if in == nil {
		return invalid("ImageConfiguration is required")
	}

	if err := validateRuntimeEnvironmentVariables(in.RuntimeEnvironmentVariables); err != nil {
		return err
	}

	if err := validateStartCommand(in.StartCommand); err != nil {
		return err
	}

	if err := validateString(in.Port); err != nil {
		return err
	}

	if err := validateRuntimeEnvironmentSecrets(in.RuntimeEnvironmentSecrets); err != nil {
		return err
	}

	return nil
}

func validateImageRepositoryType(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "ECR" && *v != "ECR_PUBLIC" {
		return invalid("Invalid ImageRepositoryType")
	}

	return nil
}

func validateImageRepository(in *api.ImageRepository) error {
	if in == nil {
		return invalid("ImageRepository is required")
	}

	if in.ImageIdentifier == nil {
		return invalid("ImageIdentifier is required")
	}

	if err := validateImageIdentifier(in.ImageIdentifier); err != nil {
		return err
	}

	if in.ImageConfiguration != nil {
		if err := validateImageConfiguration(in.ImageConfiguration); err != nil {
			return err
		}
	}

	if in.ImageRepositoryType == nil {
		return invalid("ImageRepositoryType is required")
	}

	if err := validateImageRepositoryType(in.ImageRepositoryType); err != nil {
		return err
	}

	return nil
}

func validateAppRunnerResourceArn(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 1011 ||
		!patternAppRunnerResourceArn.MatchString(*v) {
		return invalid("Invalid AppRunnerResourceArn")
	}

	return nil
}

func validateRoleArn(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 29 || utf8.RuneCountInString(*v) > 1024 || !patternRoleArn.MatchString(*v) {
		return invalid("Invalid RoleArn")
	}

	return nil
}

func validateAuthenticationConfiguration(in *api.AuthenticationConfiguration) error {
	if in == nil {
		return invalid("AuthenticationConfiguration is required")
	}

	if err := validateAppRunnerResourceArn(in.ConnectionArn); err != nil {
		return err
	}

	if err := validateRoleArn(in.AccessRoleArn); err != nil {
		return err
	}

	return nil
}

func validateSourceConfiguration(in *api.SourceConfiguration) error {
	if in == nil {
		return invalid("SourceConfiguration is required")
	}

	if in.CodeRepository != nil {
		if err := validateCodeRepository(in.CodeRepository); err != nil {
			return err
		}
	}

	if in.ImageRepository != nil {
		if err := validateImageRepository(in.ImageRepository); err != nil {
			return err
		}
	}

	if in.AuthenticationConfiguration != nil {
		if err := validateAuthenticationConfiguration(in.AuthenticationConfiguration); err != nil {
			return err
		}
	}

	return nil
}

func validateCpu(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 3 || utf8.RuneCountInString(*v) > 9 || !patternCpu.MatchString(*v) {
		return invalid("Invalid Cpu")
	}

	return nil
}

func validateMemory(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 3 || utf8.RuneCountInString(*v) > 6 || !patternMemory.MatchString(*v) {
		return invalid("Invalid Memory")
	}

	return nil
}

func validateInstanceConfiguration(in *api.InstanceConfiguration) error {
	if in == nil {
		return invalid("InstanceConfiguration is required")
	}

	if err := validateCpu(in.Cpu); err != nil {
		return err
	}

	if err := validateMemory(in.Memory); err != nil {
		return err
	}

	if err := validateRoleArn(in.InstanceRoleArn); err != nil {
		return err
	}

	return nil
}

func validateKmsKeyArn(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 0 || utf8.RuneCountInString(*v) > 256 || !patternKmsKeyArn.MatchString(*v) {
		return invalid("Invalid KmsKeyArn")
	}

	return nil
}

func validateEncryptionConfiguration(in *api.EncryptionConfiguration) error {
	if in == nil {
		return invalid("EncryptionConfiguration is required")
	}

	if in.KmsKey == nil {
		return invalid("KmsKey is required")
	}

	if err := validateKmsKeyArn(in.KmsKey); err != nil {
		return err
	}

	return nil
}

func validateHealthCheckProtocol(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "TCP" && *v != "HTTP" {
		return invalid("Invalid HealthCheckProtocol")
	}

	return nil
}

func validateHealthCheckPath(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 {
		return invalid("Invalid HealthCheckPath")
	}

	return nil
}

func validateHealthCheckInterval(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid HealthCheckInterval")
	}

	return nil
}

func validateHealthCheckTimeout(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid HealthCheckTimeout")
	}

	return nil
}

func validateHealthCheckHealthyThreshold(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid HealthCheckHealthyThreshold")
	}

	return nil
}

func validateHealthCheckUnhealthyThreshold(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid HealthCheckUnhealthyThreshold")
	}

	return nil
}

func validateHealthCheckConfiguration(in *api.HealthCheckConfiguration) error {
	if in == nil {
		return invalid("HealthCheckConfiguration is required")
	}

	if err := validateHealthCheckProtocol(in.Protocol); err != nil {
		return err
	}

	if err := validateHealthCheckPath(in.Path); err != nil {
		return err
	}

	if err := validateHealthCheckInterval(in.Interval); err != nil {
		return err
	}

	if err := validateHealthCheckTimeout(in.Timeout); err != nil {
		return err
	}

	if err := validateHealthCheckHealthyThreshold(in.HealthyThreshold); err != nil {
		return err
	}

	if err := validateHealthCheckUnhealthyThreshold(in.UnhealthyThreshold); err != nil {
		return err
	}

	return nil
}

func validateEgressType(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "DEFAULT" && *v != "VPC" {
		return invalid("Invalid EgressType")
	}

	return nil
}

func validateEgressConfiguration(in *api.EgressConfiguration) error {
	if in == nil {
		return invalid("EgressConfiguration is required")
	}

	if err := validateEgressType(in.EgressType); err != nil {
		return err
	}

	if err := validateAppRunnerResourceArn(in.VpcConnectorArn); err != nil {
		return err
	}

	return nil
}

func validateIngressConfiguration(in *api.IngressConfiguration) error {
	if in == nil {
		return invalid("IngressConfiguration is required")
	}

	return nil
}

func validateIpAddressType(v *string) error {
	if v == nil {
		return nil
	}

	if *v != "IPV4" && *v != "DUAL_STACK" {
		return invalid("Invalid IpAddressType")
	}

	return nil
}

func validateNetworkConfiguration(in *api.NetworkConfiguration) error {
	if in == nil {
		return invalid("NetworkConfiguration is required")
	}

	if in.EgressConfiguration != nil {
		if err := validateEgressConfiguration(in.EgressConfiguration); err != nil {
			return err
		}
	}

	if in.IngressConfiguration != nil {
		if err := validateIngressConfiguration(in.IngressConfiguration); err != nil {
			return err
		}
	}

	if err := validateIpAddressType(in.IpAddressType); err != nil {
		return err
	}

	return nil
}

func validateServiceObservabilityConfiguration(in *api.ServiceObservabilityConfiguration) error {
	if in == nil {
		return invalid("ServiceObservabilityConfiguration is required")
	}

	if in.ObservabilityEnabled == nil {
		return invalid("ObservabilityEnabled is required")
	}

	if err := validateAppRunnerResourceArn(in.ObservabilityConfigurationArn); err != nil {
		return err
	}

	return nil
}

func validateCreateServiceRequest(in *api.CreateServiceRequest) error {
	if in == nil {
		return invalid("CreateServiceRequest is required")
	}

	if in.ServiceName == nil {
		return invalid("ServiceName is required")
	}

	if err := validateServiceName(in.ServiceName); err != nil {
		return err
	}

	if in.SourceConfiguration == nil {
		return invalid("SourceConfiguration is required")
	}

	if in.SourceConfiguration != nil {
		if err := validateSourceConfiguration(in.SourceConfiguration); err != nil {
			return err
		}
	}

	if in.InstanceConfiguration != nil {
		if err := validateInstanceConfiguration(in.InstanceConfiguration); err != nil {
			return err
		}
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	if in.EncryptionConfiguration != nil {
		if err := validateEncryptionConfiguration(in.EncryptionConfiguration); err != nil {
			return err
		}
	}

	if in.HealthCheckConfiguration != nil {
		if err := validateHealthCheckConfiguration(in.HealthCheckConfiguration); err != nil {
			return err
		}
	}

	if err := validateAppRunnerResourceArn(in.AutoScalingConfigurationArn); err != nil {
		return err
	}

	if in.NetworkConfiguration != nil {
		if err := validateNetworkConfiguration(in.NetworkConfiguration); err != nil {
			return err
		}
	}

	if in.ObservabilityConfiguration != nil {
		if err := validateServiceObservabilityConfiguration(in.ObservabilityConfiguration); err != nil {
			return err
		}
	}

	return nil
}

func validateVpcConnectorName(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 4 || utf8.RuneCountInString(*v) > 40 || !patternVpcConnectorName.MatchString(*v) {
		return invalid("Invalid VpcConnectorName")
	}

	return nil
}

func validateStringList(v []string) error {
	if v == nil {
		return nil
	}

	for _, item := range v {
		if err := validateString(new(item)); err != nil {
			return err
		}
	}

	return nil
}

func validateCreateVpcConnectorRequest(in *api.CreateVpcConnectorRequest) error {
	if in == nil {
		return invalid("CreateVpcConnectorRequest is required")
	}

	if in.VpcConnectorName == nil {
		return invalid("VpcConnectorName is required")
	}

	if err := validateVpcConnectorName(in.VpcConnectorName); err != nil {
		return err
	}

	if in.Subnets == nil {
		return invalid("Subnets is required")
	}

	if err := validateStringList(in.Subnets); err != nil {
		return err
	}

	if err := validateStringList(in.SecurityGroups); err != nil {
		return err
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	return nil
}

func validateDeleteAutoScalingConfigurationRequest(in *api.DeleteAutoScalingConfigurationRequest) error {
	if in == nil {
		return invalid("DeleteAutoScalingConfigurationRequest is required")
	}

	if in.AutoScalingConfigurationArn == nil {
		return invalid("AutoScalingConfigurationArn is required")
	}

	if err := validateAppRunnerResourceArn(in.AutoScalingConfigurationArn); err != nil {
		return err
	}

	return nil
}

func validateDeleteConnectionRequest(in *api.DeleteConnectionRequest) error {
	if in == nil {
		return invalid("DeleteConnectionRequest is required")
	}

	if in.ConnectionArn == nil {
		return invalid("ConnectionArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ConnectionArn); err != nil {
		return err
	}

	return nil
}

func validateDeleteObservabilityConfigurationRequest(in *api.DeleteObservabilityConfigurationRequest) error {
	if in == nil {
		return invalid("DeleteObservabilityConfigurationRequest is required")
	}

	if in.ObservabilityConfigurationArn == nil {
		return invalid("ObservabilityConfigurationArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ObservabilityConfigurationArn); err != nil {
		return err
	}

	return nil
}

func validateDeleteServiceRequest(in *api.DeleteServiceRequest) error {
	if in == nil {
		return invalid("DeleteServiceRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	return nil
}

func validateDeleteVpcConnectorRequest(in *api.DeleteVpcConnectorRequest) error {
	if in == nil {
		return invalid("DeleteVpcConnectorRequest is required")
	}

	if in.VpcConnectorArn == nil {
		return invalid("VpcConnectorArn is required")
	}

	if err := validateAppRunnerResourceArn(in.VpcConnectorArn); err != nil {
		return err
	}

	return nil
}

func validateDescribeAutoScalingConfigurationRequest(in *api.DescribeAutoScalingConfigurationRequest) error {
	if in == nil {
		return invalid("DescribeAutoScalingConfigurationRequest is required")
	}

	if in.AutoScalingConfigurationArn == nil {
		return invalid("AutoScalingConfigurationArn is required")
	}

	if err := validateAppRunnerResourceArn(in.AutoScalingConfigurationArn); err != nil {
		return err
	}

	return nil
}

func validateDescribeObservabilityConfigurationRequest(in *api.DescribeObservabilityConfigurationRequest) error {
	if in == nil {
		return invalid("DescribeObservabilityConfigurationRequest is required")
	}

	if in.ObservabilityConfigurationArn == nil {
		return invalid("ObservabilityConfigurationArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ObservabilityConfigurationArn); err != nil {
		return err
	}

	return nil
}

func validateDescribeServiceRequest(in *api.DescribeServiceRequest) error {
	if in == nil {
		return invalid("DescribeServiceRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	return nil
}

func validateDescribeVpcConnectorRequest(in *api.DescribeVpcConnectorRequest) error {
	if in == nil {
		return invalid("DescribeVpcConnectorRequest is required")
	}

	if in.VpcConnectorArn == nil {
		return invalid("VpcConnectorArn is required")
	}

	if err := validateAppRunnerResourceArn(in.VpcConnectorArn); err != nil {
		return err
	}

	return nil
}

func validateMaxResults(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 100 {
		return invalid("Invalid MaxResults")
	}

	return nil
}

func validateNextToken(v *string) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) < 1 || utf8.RuneCountInString(*v) > 1024 || !patternNextToken.MatchString(*v) {
		return invalid("Invalid NextToken")
	}

	return nil
}

func validateListAutoScalingConfigurationsRequest(in *api.ListAutoScalingConfigurationsRequest) error {
	if in == nil {
		return invalid("ListAutoScalingConfigurationsRequest is required")
	}

	if err := validateAutoScalingConfigurationName(in.AutoScalingConfigurationName); err != nil {
		return err
	}

	if err := validateMaxResults(in.MaxResults); err != nil {
		return err
	}

	if err := validateNextToken(in.NextToken); err != nil {
		return err
	}

	return nil
}

func validateListConnectionsRequest(in *api.ListConnectionsRequest) error {
	if in == nil {
		return invalid("ListConnectionsRequest is required")
	}

	if err := validateConnectionName(in.ConnectionName); err != nil {
		return err
	}

	if err := validateMaxResults(in.MaxResults); err != nil {
		return err
	}

	if err := validateNextToken(in.NextToken); err != nil {
		return err
	}

	return nil
}

func validateListObservabilityConfigurationsRequest(in *api.ListObservabilityConfigurationsRequest) error {
	if in == nil {
		return invalid("ListObservabilityConfigurationsRequest is required")
	}

	if err := validateObservabilityConfigurationName(in.ObservabilityConfigurationName); err != nil {
		return err
	}

	if err := validateMaxResults(in.MaxResults); err != nil {
		return err
	}

	if err := validateNextToken(in.NextToken); err != nil {
		return err
	}

	return nil
}

func validateListOperationsMaxResults(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid ListOperationsMaxResults")
	}

	return nil
}

func validateListOperationsRequest(in *api.ListOperationsRequest) error {
	if in == nil {
		return invalid("ListOperationsRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	if err := validateString(in.NextToken); err != nil {
		return err
	}

	if err := validateListOperationsMaxResults(in.MaxResults); err != nil {
		return err
	}

	return nil
}

func validateServiceMaxResults(v *int64) error {
	if v == nil {
		return nil
	}

	if *v < 1 || *v > 20 {
		return invalid("Invalid ServiceMaxResults")
	}

	return nil
}

func validateListServicesRequest(in *api.ListServicesRequest) error {
	if in == nil {
		return invalid("ListServicesRequest is required")
	}

	if err := validateString(in.NextToken); err != nil {
		return err
	}

	if err := validateServiceMaxResults(in.MaxResults); err != nil {
		return err
	}

	return nil
}

func validateListTagsForResourceRequest(in *api.ListTagsForResourceRequest) error {
	if in == nil {
		return invalid("ListTagsForResourceRequest is required")
	}

	if in.ResourceArn == nil {
		return invalid("ResourceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ResourceArn); err != nil {
		return err
	}

	return nil
}

func validateListVpcConnectorsRequest(in *api.ListVpcConnectorsRequest) error {
	if in == nil {
		return invalid("ListVpcConnectorsRequest is required")
	}

	if err := validateMaxResults(in.MaxResults); err != nil {
		return err
	}

	if err := validateNextToken(in.NextToken); err != nil {
		return err
	}

	return nil
}

func validatePauseServiceRequest(in *api.PauseServiceRequest) error {
	if in == nil {
		return invalid("PauseServiceRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	return nil
}

func validateResumeServiceRequest(in *api.ResumeServiceRequest) error {
	if in == nil {
		return invalid("ResumeServiceRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	return nil
}

func validateStartDeploymentRequest(in *api.StartDeploymentRequest) error {
	if in == nil {
		return invalid("StartDeploymentRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	return nil
}

func validateTagResourceRequest(in *api.TagResourceRequest) error {
	if in == nil {
		return invalid("TagResourceRequest is required")
	}

	if in.ResourceArn == nil {
		return invalid("ResourceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ResourceArn); err != nil {
		return err
	}

	if in.Tags == nil {
		return invalid("Tags is required")
	}

	if err := validateTagList(in.Tags); err != nil {
		return err
	}

	return nil
}

func validateTagKeyList(v []string) error {
	if v == nil {
		return nil
	}

	for _, item := range v {
		if err := validateTagKey(new(item)); err != nil {
			return err
		}
	}

	return nil
}

func validateUntagResourceRequest(in *api.UntagResourceRequest) error {
	if in == nil {
		return invalid("UntagResourceRequest is required")
	}

	if in.ResourceArn == nil {
		return invalid("ResourceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ResourceArn); err != nil {
		return err
	}

	if in.TagKeys == nil {
		return invalid("TagKeys is required")
	}

	if err := validateTagKeyList(in.TagKeys); err != nil {
		return err
	}

	return nil
}

func validateUpdateServiceRequest(in *api.UpdateServiceRequest) error {
	if in == nil {
		return invalid("UpdateServiceRequest is required")
	}

	if in.ServiceArn == nil {
		return invalid("ServiceArn is required")
	}

	if err := validateAppRunnerResourceArn(in.ServiceArn); err != nil {
		return err
	}

	if in.SourceConfiguration != nil {
		if err := validateSourceConfiguration(in.SourceConfiguration); err != nil {
			return err
		}
	}

	if in.InstanceConfiguration != nil {
		if err := validateInstanceConfiguration(in.InstanceConfiguration); err != nil {
			return err
		}
	}

	if err := validateAppRunnerResourceArn(in.AutoScalingConfigurationArn); err != nil {
		return err
	}

	if in.HealthCheckConfiguration != nil {
		if err := validateHealthCheckConfiguration(in.HealthCheckConfiguration); err != nil {
			return err
		}
	}

	if in.NetworkConfiguration != nil {
		if err := validateNetworkConfiguration(in.NetworkConfiguration); err != nil {
			return err
		}
	}

	if in.ObservabilityConfiguration != nil {
		if err := validateServiceObservabilityConfiguration(in.ObservabilityConfiguration); err != nil {
			return err
		}
	}

	return nil
}

func validateUpdateDefaultAutoScalingConfigurationRequest(in *api.UpdateDefaultAutoScalingConfigurationRequest) error {
	if in == nil {
		return invalid("UpdateDefaultAutoScalingConfigurationRequest is required")
	}

	if in.AutoScalingConfigurationArn == nil {
		return invalid("AutoScalingConfigurationArn is required")
	}

	if err := validateAppRunnerResourceArn(in.AutoScalingConfigurationArn); err != nil {
		return err
	}

	return nil
}
