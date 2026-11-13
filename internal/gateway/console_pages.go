package gateway

import "strings"

// consolePage is what a dedicated console page may do with one service: the operations on its allowlist, and
// whether the service is global, so actions carry no Region.
type consolePage struct {
	operations []string
	global     bool
}

// consolePages lists, by service name, every service the console calls. Nothing outside it is reachable through
// /_tinystack/actions except the inventory deletes in consoleResourceDeletes.
var consolePages = map[string]consolePage{
	"acm": {operations: strings.Fields(`
 RequestCertificate DescribeCertificate ListCertificates DeleteCertificate GetCertificate ImportCertificate
 ExportCertificate AddTagsToCertificate RemoveTagsFromCertificate ListTagsForCertificate UpdateCertificateOptions
`)},
	"apigateway": {operations: strings.Fields(`
	CreateRestApi GetRestApi GetRestApis UpdateRestApi DeleteRestApi
	CreateResource GetResource GetResources UpdateResource DeleteResource
	PutMethod GetMethod UpdateMethod DeleteMethod
	PutIntegration GetIntegration UpdateIntegration DeleteIntegration
	CreateStage GetStage GetStages UpdateStage DeleteStage
	CreateDeployment GetDeployment GetDeployments UpdateDeployment DeleteDeployment
	CreateAuthorizer GetAuthorizer GetAuthorizers UpdateAuthorizer DeleteAuthorizer
	GetTags TagResource UntagResource InvokeApi
`)},
	"apigatewayv2": {operations: strings.Fields(`
	CreateApi GetApi GetApis UpdateApi DeleteApi DeleteCorsConfiguration
	CreateRoute GetRoute GetRoutes UpdateRoute DeleteRoute
	CreateIntegration GetIntegration GetIntegrations UpdateIntegration DeleteIntegration
	CreateStage GetStage GetStages UpdateStage DeleteStage
	CreateDeployment GetDeployment GetDeployments UpdateDeployment DeleteDeployment
	CreateAuthorizer GetAuthorizer GetAuthorizers UpdateAuthorizer DeleteAuthorizer
	GetTags TagResource UntagResource InvokeApi
`)},
	// AppConfig exposes the hosted configuration operations the page uses; the data plane serves its fetch panel.
	"appconfig": {operations: strings.Fields(`
 CreateApplication GetApplication UpdateApplication DeleteApplication ListApplications
 CreateEnvironment GetEnvironment UpdateEnvironment DeleteEnvironment ListEnvironments
 CreateConfigurationProfile GetConfigurationProfile UpdateConfigurationProfile DeleteConfigurationProfile
 ListConfigurationProfiles
 CreateDeploymentStrategy UpdateDeploymentStrategy DeleteDeploymentStrategy ListDeploymentStrategies
 CreateHostedConfigurationVersion GetHostedConfigurationVersion ListHostedConfigurationVersions
 DeleteHostedConfigurationVersion
 StartDeployment GetDeployment ListDeployments StopDeployment
 TagResource UntagResource ListTagsForResource
`)},
	"appconfigdata": {operations: strings.Fields(`StartConfigurationSession GetLatestConfiguration`)},
	// AppSync exposes only the GraphQL console workflows.
	"appsync": {operations: strings.Fields(`
 CreateGraphqlApi GetGraphqlApi UpdateGraphqlApi DeleteGraphqlApi ListGraphqlApis
 StartSchemaCreation GetSchemaCreationStatus GetIntrospectionSchema ListTypes
 CreateDataSource UpdateDataSource DeleteDataSource ListDataSources
 CreateResolver UpdateResolver DeleteResolver ListResolvers
 CreateFunction UpdateFunction DeleteFunction ListFunctions
 CreateApiKey ListApiKeys DeleteApiKey
 TagResource UntagResource
`)},
	"athena": {operations: strings.Fields(`
 StartQueryExecution GetQueryExecution GetQueryResults StopQueryExecution ListQueryExecutions BatchGetQueryExecution
 CreateWorkGroup GetWorkGroup ListWorkGroups UpdateWorkGroup DeleteWorkGroup
 CreateNamedQuery GetNamedQuery ListNamedQueries DeleteNamedQuery BatchGetNamedQuery
 GetDataCatalog ListDataCatalogs ListDatabases GetDatabase ListTableMetadata GetTableMetadata
 TagResource UntagResource ListTagsForResource
`)},
	// Batch exposes every operation the service implements: environments, queues, definitions, jobs and tags.
	"batch": {operations: strings.Fields(`
 CreateComputeEnvironment DescribeComputeEnvironments UpdateComputeEnvironment DeleteComputeEnvironment
 CreateJobQueue DescribeJobQueues UpdateJobQueue DeleteJobQueue
 RegisterJobDefinition DescribeJobDefinitions DeregisterJobDefinition
 SubmitJob DescribeJobs ListJobs CancelJob TerminateJob
 TagResource UntagResource ListTagsForResource
`)},
	"budgets": {operations: strings.Fields(`
	CreateBudget
	DescribeBudget
	DescribeBudgets
	UpdateBudget
	DeleteBudget
	CreateNotification
	UpdateNotification
	DeleteNotification
	DescribeNotificationsForBudget
	CreateSubscriber
	UpdateSubscriber
	DeleteSubscriber
	DescribeSubscribersForNotification
	TagResource
	UntagResource
	ListTagsForResource
`), global: true},
	consoleCloudFormation: {operations: strings.Fields(`
 ListStacks DescribeStacks DescribeStackEvents ListStackResources GetTemplate
 ListChangeSets DescribeChangeSet ExecuteChangeSet DeleteChangeSet DeleteStack
 CreateStack UpdateStack CreateChangeSet ValidateTemplate GetTemplateSummary UpdateTerminationProtection
 DescribeStackResource DescribeStackResources ListExports ListImports
`)},
	"cloudfront": {operations: strings.Fields(`
 CreateDistributionWithTags GetDistribution GetDistributionConfig ListDistributions UpdateDistribution
 DeleteDistribution CreateInvalidation GetInvalidation ListInvalidations
 CreateOriginAccessControl GetOriginAccessControl GetOriginAccessControlConfig ListOriginAccessControls
 UpdateOriginAccessControl DeleteOriginAccessControl
 CreateKeyValueStore DescribeKeyValueStore ListKeyValueStores UpdateKeyValueStore DeleteKeyValueStore
 TagResource UntagResource ListTagsForResource
`), global: true},
	"cloudtrail": {operations: strings.Fields(`
 CreateTrail GetTrail DescribeTrails UpdateTrail DeleteTrail GetTrailStatus StartLogging StopLogging
 PutEventSelectors GetEventSelectors AddTags RemoveTags ListTags LookupEvents
`)},
	"cloudwatch": {operations: strings.Fields(`
	ListMetrics GetMetricData GetMetricStatistics PutMetricData
	PutMetricAlarm PutCompositeAlarm DescribeAlarms DescribeAlarmsForMetric DescribeAlarmHistory
	DeleteAlarms SetAlarmState EnableAlarmActions DisableAlarmActions
	PutDashboard GetDashboard ListDashboards DeleteDashboards
	TagResource UntagResource ListTagsForResource
`)},
	"codebuild": {operations: strings.Fields(`
 CreateProject BatchGetProjects ListProjects UpdateProject DeleteProject
 StartBuild BatchGetBuilds StopBuild ListBuilds ListBuildsForProject BatchDeleteBuilds RetryBuild
 CreateReportGroup UpdateReportGroup DeleteReportGroup BatchGetReportGroups ListReportGroups
 ListReports ListReportsForReportGroup BatchGetReports DescribeTestCases
`)},
	// Cognito identity pools exposes pool, role, identity and tag management, plus the two calls the test panel makes for
	// an unauthenticated identity. Token issue and login linking stay API-only.
	"cognitoidentity": {operations: strings.Fields(`
 CreateIdentityPool DescribeIdentityPool UpdateIdentityPool DeleteIdentityPool ListIdentityPools
 SetIdentityPoolRoles GetIdentityPoolRoles ListIdentities DescribeIdentity DeleteIdentities
 GetId GetCredentialsForIdentity TagResource UntagResource ListTagsForResource
`)},
	// Cognito exposes only operations used by the user pools console.
	consoleCognito: {operations: strings.Fields(`
 ListUserPools DescribeUserPool ListUsers AdminGetUser AdminCreateUser AdminConfirmSignUp
 AdminEnableUser AdminDisableUser AdminResetUserPassword AdminSetUserPassword AdminDeleteUser
 AdminUpdateUserAttributes AdminListGroupsForUser AdminAddUserToGroup AdminRemoveUserFromGroup
 ListGroups CreateGroup DeleteGroup ListUsersInGroup ListUserPoolClients DescribeUserPoolClient
 DescribeUserPoolDomain CreateUserPool UpdateUserPool DeleteUserPool
 GetUserPoolMfaConfig SetUserPoolMfaConfig AddCustomAttributes TagResource UntagResource ListTagsForResource
 CreateUserPoolClient UpdateUserPoolClient DeleteUserPoolClient
 CreateUserPoolDomain UpdateUserPoolDomain DeleteUserPoolDomain GetGroup UpdateGroup
 AdminDeleteUserAttributes AdminUserGlobalSignOut
`)},
	"costandusagereportservice": {operations: strings.Fields(`
	DeleteReportDefinition
	DescribeReportDefinitions
	ListTagsForResource
	ModifyReportDefinition
	PutReportDefinition
	TagResource
	UntagResource
`), global: true},
	"costexplorer": {operations: strings.Fields(`
	GetCostAndUsage
	GetDimensionValues
	GetTags
	CreateAnomalyMonitor
	CreateAnomalySubscription
	CreateCostCategoryDefinition
	DeleteAnomalyMonitor
	DeleteAnomalySubscription
	DeleteCostCategoryDefinition
	DescribeCostCategoryDefinition
	GetAnomalyMonitors
	GetAnomalySubscriptions
	ListCostCategoryDefinitions
	ListTagsForResource
	TagResource
	UntagResource
	UpdateAnomalyMonitor
	UpdateAnomalySubscription
	UpdateCostCategoryDefinition
`), global: true},
	// Aurora DSQL exposes the cluster and tag operations the page uses; cluster policies stay API-only.
	"dsql": {operations: strings.Fields(`
 CreateCluster GetCluster ListClusters UpdateCluster DeleteCluster TagResource UntagResource
`)},
	"dynamodb": {operations: strings.Fields(`
	CreateTable DeleteTable DescribeTable ListTables UpdateTable
	GetItem PutItem UpdateItem DeleteItem Query Scan BatchWriteItem
	CreateBackup ListBackups DescribeBackup DeleteBackup RestoreTableFromBackup
	DescribeContinuousBackups UpdateContinuousBackups RestoreTableToPointInTime
	ExportTableToPointInTime ListExports DescribeExport ExecuteStatement
	DescribeKinesisStreamingDestination EnableKinesisStreamingDestination
	DisableKinesisStreamingDestination UpdateKinesisStreamingDestination
	DescribeTimeToLive UpdateTimeToLive ListTagsOfResource TagResource UntagResource
`)},
	"ec2": {operations: strings.Fields(`
 DescribeAvailabilityZones DescribeImages DescribeInstances DescribeInstanceStatus
 RunInstances StartInstances StopInstances RebootInstances TerminateInstances ModifyInstanceMetadataOptions
 DescribeIamInstanceProfileAssociations AssociateIamInstanceProfile ReplaceIamInstanceProfileAssociation
 DisassociateIamInstanceProfile DescribeKeyPairs CreateKeyPair ImportKeyPair DeleteKeyPair
 DescribeVpcs CreateVpc DeleteVpc DescribeVpcAttribute ModifyVpcAttribute
 DescribeSubnets CreateSubnet DeleteSubnet ModifySubnetAttribute
 DescribeSecurityGroups CreateSecurityGroup DeleteSecurityGroup DescribeSecurityGroupRules
 AuthorizeSecurityGroupIngress AuthorizeSecurityGroupEgress RevokeSecurityGroupIngress RevokeSecurityGroupEgress
 DescribeInternetGateways CreateInternetGateway AttachInternetGateway DetachInternetGateway DeleteInternetGateway
 DescribeRouteTables CreateRouteTable DeleteRouteTable CreateRoute ReplaceRoute DeleteRoute
 AssociateRouteTable DisassociateRouteTable ReplaceRouteTableAssociation
 DescribeAddresses AllocateAddress AssociateAddress DisassociateAddress ReleaseAddress
 DescribeNatGateways CreateNatGateway DeleteNatGateway
 DescribeNetworkAcls CreateNetworkAcl DeleteNetworkAcl CreateNetworkAclEntry ReplaceNetworkAclEntry
 DeleteNetworkAclEntry ReplaceNetworkAclAssociation
 DescribeVpcEndpoints CreateVpcEndpoint ModifyVpcEndpoint DeleteVpcEndpoints DescribeNetworkInterfaces
 CreateTags DeleteTags DescribeTags
`)},
	consoleECR: {operations: strings.Fields(`
 CreateRepository DescribeRepositories DeleteRepository PutImageTagMutability PutImageScanningConfiguration
 DescribeImages BatchDeleteImage GetRepositoryPolicy SetRepositoryPolicy DeleteRepositoryPolicy
 GetLifecyclePolicy PutLifecyclePolicy DeleteLifecyclePolicy ListTagsForResource TagResource UntagResource
`)},
	"ecs": {operations: strings.Fields(`
 CreateCluster ListClusters DescribeClusters DeleteCluster RegisterTaskDefinition ListTaskDefinitions
 DescribeTaskDefinition DeregisterTaskDefinition CreateService ListServices DescribeServices UpdateService DeleteService
 RunTask ListTasks DescribeTasks StopTask
`)},
	// EFS exposes the file system, mount target, access point and tag operations the page uses.
	"efs": {operations: strings.Fields(`
 CreateFileSystem DescribeFileSystems UpdateFileSystem DeleteFileSystem
 CreateMountTarget DescribeMountTargets DeleteMountTarget
 DescribeMountTargetSecurityGroups ModifyMountTargetSecurityGroups
 CreateAccessPoint DescribeAccessPoints DeleteAccessPoint
 TagResource UntagResource
`)},
	"eks": {operations: strings.Fields(`
 CreateCluster DescribeCluster ListClusters DeleteCluster UpdateClusterConfig UpdateClusterVersion
 DescribeClusterVersions
 CreateNodegroup DescribeNodegroup ListNodegroups DeleteNodegroup UpdateNodegroupConfig
 CreateAddon DescribeAddon ListAddons DeleteAddon DescribeAddonVersions
 CreateFargateProfile DescribeFargateProfile ListFargateProfiles DeleteFargateProfile
 CreateAccessEntry DescribeAccessEntry ListAccessEntries DeleteAccessEntry
 AssociateAccessPolicy DisassociateAccessPolicy ListAssociatedAccessPolicies ListAccessPolicies
 TagResource UntagResource
`)},
	"elasticache": {operations: strings.Fields(`
 CreateCacheCluster DescribeCacheClusters ModifyCacheCluster DeleteCacheCluster RebootCacheCluster
 CreateReplicationGroup DescribeReplicationGroups ModifyReplicationGroup DeleteReplicationGroup
 IncreaseReplicaCount DecreaseReplicaCount TestFailover
 CreateServerlessCache DescribeServerlessCaches ModifyServerlessCache DeleteServerlessCache
 CreateCacheSubnetGroup DescribeCacheSubnetGroups DeleteCacheSubnetGroup
 CreateCacheParameterGroup DescribeCacheParameterGroups DescribeCacheParameters ModifyCacheParameterGroup
 DeleteCacheParameterGroup DescribeCacheEngineVersions DescribeEvents
 AddTagsToResource RemoveTagsFromResource ListTagsForResource
`)},
	"elbv2": {operations: strings.Fields(`
 CreateLoadBalancer DescribeLoadBalancers DeleteLoadBalancer DescribeLoadBalancerAttributes
 ModifyLoadBalancerAttributes SetSecurityGroups SetSubnets
 CreateTargetGroup DescribeTargetGroups ModifyTargetGroup DeleteTargetGroup
 DescribeTargetGroupAttributes ModifyTargetGroupAttributes RegisterTargets DeregisterTargets DescribeTargetHealth
 CreateListener DescribeListeners ModifyListener DeleteListener DescribeListenerAttributes ModifyListenerAttributes
 AddListenerCertificates DescribeListenerCertificates RemoveListenerCertificates
 CreateRule DescribeRules ModifyRule DeleteRule SetRulePriorities
 AddTags RemoveTags DescribeTags DescribeSSLPolicies
`)},
	"eventbridge": {operations: strings.Fields(`
	CreateEventBus UpdateEventBus DeleteEventBus DescribeEventBus ListEventBuses
	PutRule DescribeRule ListRules EnableRule DisableRule DeleteRule
	PutTargets RemoveTargets ListTargetsByRule TestEventPattern PutEvents
	TagResource UntagResource ListTagsForResource
`)},
	"firehose": {operations: strings.Fields(`
 ListDeliveryStreams CreateDeliveryStream DescribeDeliveryStream UpdateDestination DeleteDeliveryStream
 PutRecord ListTagsForDeliveryStream TagDeliveryStream UntagDeliveryStream
`)},
	"glue": {operations: strings.Fields(`
CreateDatabase GetDatabase GetDatabases UpdateDatabase DeleteDatabase
CreateTable GetTable GetTables UpdateTable DeleteTable
CreatePartition GetPartition GetPartitions DeletePartition
CreateCrawler GetCrawler GetCrawlers UpdateCrawler DeleteCrawler StartCrawler StopCrawler
CreateJob GetJob GetJobs UpdateJob DeleteJob StartJobRun GetJobRuns
CreateTrigger GetTrigger GetTriggers UpdateTrigger DeleteTrigger StartTrigger StopTrigger
CreateWorkflow GetWorkflow ListWorkflows UpdateWorkflow DeleteWorkflow
TagResource UntagResource GetTags
`)},
	// Console actions are an explicit subset of the registered SDK operations.
	// IAM and cost definitions are account-wide; the region is only used when verifying signatures.
	"iam": {operations: strings.Fields(`
	CreateUser GetUser UpdateUser DeleteUser TagUser UntagUser
	CreateGroup GetGroup UpdateGroup DeleteGroup AddUserToGroup RemoveUserFromGroup ListGroupsForUser
	CreateRole GetRole UpdateRole DeleteRole UpdateAssumeRolePolicy TagRole UntagRole
	CreatePolicy GetPolicy DeletePolicy CreatePolicyVersion GetPolicyVersion ListPolicyVersions
	DeletePolicyVersion SetDefaultPolicyVersion TagPolicy UntagPolicy
	AttachUserPolicy DetachUserPolicy PutUserPolicy GetUserPolicy DeleteUserPolicy
	AttachGroupPolicy DetachGroupPolicy PutGroupPolicy GetGroupPolicy DeleteGroupPolicy
	AttachRolePolicy DetachRolePolicy PutRolePolicy GetRolePolicy DeleteRolePolicy
	CreateAccessKey ListAccessKeys UpdateAccessKey DeleteAccessKey
	CreateInstanceProfile GetInstanceProfile DeleteInstanceProfile AddRoleToInstanceProfile
	RemoveRoleFromInstanceProfile ListInstanceProfilesForRole
	SimulatePrincipalPolicy SimulateCustomPolicy
`), global: true},
	"kafka": {operations: strings.Fields(`
 CreateCluster DescribeCluster ListClusters DeleteCluster UpdateMonitoring
 GetBootstrapBrokers ListNodes ListTagsForResource TagResource UntagResource
 ListClustersV2 DescribeClusterV2 UpdateBrokerCount UpdateBrokerStorage
 UpdateClusterConfiguration UpdateClusterKafkaVersion
 CreateConfiguration DescribeConfiguration DescribeConfigurationRevision ListConfigurations
 ListConfigurationRevisions UpdateConfiguration DeleteConfiguration
`)},
	"kinesis": {operations: strings.Fields(`
 ListStreams CreateStream DeleteStream DescribeStream ListShards UpdateShardCount
 IncreaseStreamRetentionPeriod DecreaseStreamRetentionPeriod StartStreamEncryption StopStreamEncryption
 EnableEnhancedMonitoring DisableEnhancedMonitoring ListTagsForStream AddTagsToStream RemoveTagsFromStream
 PutRecord GetShardIterator GetRecords
`)},
	"kms": {operations: strings.Fields(`
 CreateKey DescribeKey ListKeys EnableKey DisableKey ScheduleKeyDeletion CancelKeyDeletion UpdateKeyDescription
 GetKeyPolicy PutKeyPolicy GetKeyRotationStatus EnableKeyRotation DisableKeyRotation
 CreateAlias UpdateAlias DeleteAlias ListAliases TagResource UntagResource ListResourceTags Encrypt Decrypt
`)},
	consoleLambda: {operations: strings.Fields(`
	CreateFunction DeleteFunction GetFunction ListFunctions
	UpdateFunctionCode UpdateFunctionConfiguration Invoke
	CreateEventSourceMapping GetEventSourceMapping ListEventSourceMappings
	UpdateEventSourceMapping DeleteEventSourceMapping
	ListTags TagResource UntagResource
	CreateFunctionUrlConfig GetFunctionUrlConfig UpdateFunctionUrlConfig
	DeleteFunctionUrlConfig ListFunctionUrlConfigs
	AddPermission GetPolicy RemovePermission
`)},
	"logs": {operations: strings.Fields(`
	CreateLogGroup DeleteLogGroup DescribeLogGroups
	CreateLogStream DeleteLogStream DescribeLogStreams GetLogEvents FilterLogEvents PutLogEvents
	PutRetentionPolicy DeleteRetentionPolicy TagResource UntagResource ListTagsForResource
	StartQuery GetQueryResults DescribeQueries
`)},
	"memorydb": {operations: strings.Fields(`
 CreateCluster DescribeClusters UpdateCluster DeleteCluster
 CreateUser DescribeUsers UpdateUser DeleteUser
 CreateACL DescribeACLs UpdateACL DeleteACL
 CreateSubnetGroup DescribeSubnetGroups UpdateSubnetGroup DeleteSubnetGroup
 CreateParameterGroup DescribeParameterGroups DescribeParameters
 UpdateParameterGroup ResetParameterGroup DeleteParameterGroup
 ListTags TagResource UntagResource
`)},
	"mq": {operations: strings.Fields(`
 CreateBroker DescribeBroker ListBrokers UpdateBroker DeleteBroker RebootBroker CreateTags ListTags DeleteTags
 CreateUser DescribeUser ListUsers UpdateUser DeleteUser
 CreateConfiguration DescribeConfiguration ListConfigurations UpdateConfiguration DescribeConfigurationRevision
 ListConfigurationRevisions DeleteConfiguration
`)},
	// OpenSearch exposes the domain and tag operations the page uses; the engine's search API is not reachable here.
	"opensearch": {operations: strings.Fields(`
 CreateDomain DescribeDomain DescribeDomains DescribeDomainConfig ListDomainNames
 ListVersions GetCompatibleVersions UpgradeDomain GetUpgradeStatus GetUpgradeHistory
 UpdateDomainConfig DeleteDomain AddTags ListTags RemoveTags
`)},
	"pipes": {operations: strings.Fields(`
 CreatePipe DescribePipe UpdatePipe DeletePipe ListPipes StartPipe StopPipe
 TagResource UntagResource ListTagsForResource
`)},
	consoleRDS: {operations: strings.Fields(`
 CreateDBInstance DescribeDBInstances ModifyDBInstance DeleteDBInstance StopDBInstance StartDBInstance RebootDBInstance
 DescribeDBClusters CreateDBCluster ModifyDBCluster DeleteDBCluster StartDBCluster StopDBCluster
 EnableHttpEndpoint DisableHttpEndpoint
 DescribeDBSnapshots CreateDBSnapshot CopyDBSnapshot DeleteDBSnapshot RestoreDBInstanceFromDBSnapshot
 DescribeDBClusterSnapshots CreateDBClusterSnapshot DeleteDBClusterSnapshot
 CreateDBSubnetGroup DescribeDBSubnetGroups DeleteDBSubnetGroup
 CreateDBParameterGroup DescribeDBParameterGroups DeleteDBParameterGroup
 DescribeDBParameters ModifyDBParameterGroup ResetDBParameterGroup
 CreateDBClusterParameterGroup DescribeDBClusterParameterGroups DeleteDBClusterParameterGroup
 DescribeDBClusterParameters ModifyDBClusterParameterGroup ResetDBClusterParameterGroup
 AddTagsToResource RemoveTagsFromResource ListTagsForResource
`)},
	// Only the tagging writes are exposed; discovery remains on the inventory endpoint.
	"resourcegroupstaggingapi": {operations: strings.Fields(`TagResources UntagResources`)},
	"route53": {operations: strings.Fields(`
 ListHostedZones GetHostedZone CreateHostedZone DeleteHostedZone UpdateHostedZoneComment
 ListResourceRecordSets ChangeResourceRecordSets
 CreateHealthCheck ListHealthChecks DeleteHealthCheck
 ChangeTagsForResource ListTagsForResource
`), global: true},
	"s3": {operations: strings.Fields(`
	CreateBucket DeleteBucket HeadBucket ListBuckets
	ListObjectsV2 ListObjectVersions HeadObject GetObject PutObject CopyObject DeleteObject DeleteObjects
	ListMultipartUploads AbortMultipartUpload
	GetBucketVersioning PutBucketVersioning GetBucketTagging PutBucketTagging DeleteBucketTagging
	GetObjectTagging PutObjectTagging DeleteObjectTagging
	GetBucketPolicy PutBucketPolicy DeleteBucketPolicy
	GetBucketCors PutBucketCors DeleteBucketCors
	GetBucketEncryption PutBucketEncryption DeleteBucketEncryption
	GetPublicAccessBlock PutPublicAccessBlock DeletePublicAccessBlock
	GetBucketOwnershipControls PutBucketOwnershipControls DeleteBucketOwnershipControls
	GetBucketLifecycleConfiguration PutBucketLifecycleConfiguration DeleteBucketLifecycle
	GetBucketWebsite PutBucketWebsite DeleteBucketWebsite
	GetBucketNotificationConfiguration PutBucketNotificationConfiguration
`)},
	"scheduler": {operations: strings.Fields(`
 CreateScheduleGroup GetScheduleGroup ListScheduleGroups DeleteScheduleGroup
 CreateSchedule GetSchedule ListSchedules UpdateSchedule DeleteSchedule
 TagResource UntagResource ListTagsForResource
`)},
	"secretsmanager": {operations: strings.Fields(`
 CreateSecret DescribeSecret ListSecrets UpdateSecret DeleteSecret RestoreSecret
 PutSecretValue GetSecretValue ListSecretVersionIds UpdateSecretVersionStage
 TagResource UntagResource GetResourcePolicy ValidateResourcePolicy PutResourcePolicy DeleteResourcePolicy
 RotateSecret CancelRotateSecret GetRandomPassword
`)},
	"servicediscovery": {operations: strings.Fields(`
 CreateHttpNamespace CreatePrivateDnsNamespace CreatePublicDnsNamespace
 GetOperation ListOperations GetNamespace ListNamespaces DeleteNamespace UpdateHttpNamespace
 CreateService GetService ListServices UpdateService DeleteService
 RegisterInstance DeregisterInstance GetInstance ListInstances GetInstancesHealthStatus
 UpdateInstanceCustomHealthStatus DiscoverInstances DiscoverInstancesRevision
 TagResource UntagResource ListTagsForResource
`)},
	"sesv2": {operations: strings.Fields(`
 PutConfigurationSetDeliveryOptions PutConfigurationSetReputationOptions PutConfigurationSetSendingOptions
 PutConfigurationSetTrackingOptions PutConfigurationSetSuppressionOptions PutEmailIdentityConfigurationSetAttributes
 CreateEmailIdentity GetEmailIdentity ListEmailIdentities DeleteEmailIdentity
 CreateEmailTemplate GetEmailTemplate UpdateEmailTemplate DeleteEmailTemplate ListEmailTemplates TestRenderEmailTemplate
 CreateConfigurationSet GetConfigurationSet ListConfigurationSets DeleteConfigurationSet
 CreateConfigurationSetEventDestination GetConfigurationSetEventDestinations
 UpdateConfigurationSetEventDestination DeleteConfigurationSetEventDestination
 SendEmail GetAccount TagResource UntagResource ListTagsForResource
`)},
	"sfn": {operations: strings.Fields(`
	CreateStateMachine DescribeStateMachine ListStateMachines UpdateStateMachine DeleteStateMachine
	ValidateStateMachineDefinition StartExecution StartSyncExecution DescribeExecution ListExecutions
	StopExecution GetExecutionHistory ListTagsForResource TagResource UntagResource
`)},
	"sns": {operations: strings.Fields(`
	CreateTopic DeleteTopic ListTopics GetTopicAttributes SetTopicAttributes
	Subscribe ConfirmSubscription Unsubscribe ListSubscriptions ListSubscriptionsByTopic
	GetSubscriptionAttributes SetSubscriptionAttributes Publish
	TagResource UntagResource ListTagsForResource
`)},
	"sqs": {operations: strings.Fields(`
	CreateQueue DeleteQueue GetQueueUrl ListQueues GetQueueAttributes SetQueueAttributes PurgeQueue
	SendMessage ReceiveMessage DeleteMessage ChangeMessageVisibility
	TagQueue UntagQueue ListQueueTags
	ListDeadLetterSourceQueues StartMessageMoveTask ListMessageMoveTasks CancelMessageMoveTask
`)},
	consoleSSM: {operations: strings.Fields(`
 DescribeParameters GetParameter GetParameterHistory PutParameter DeleteParameter DeleteParameters
 LabelParameterVersion UnlabelParameterVersion ListTagsForResource AddTagsToResource RemoveTagsFromResource
`)},
	consoleSTS: {operations: strings.Fields(`GetCallerIdentity`)},
}
