package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleBatchLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"

	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, service, operation, account, region, input)

		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}
	batch := func(operation string, input map[string]any) map[string]any {
		t.Helper()

		return call("batch", operation, input)
	}
	failure := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "batch", operation, account, region, input)

		return consoleResponse(t, srv, req, http.StatusBadRequest)
	}
	list := func(operation, key string, input map[string]any) []any {
		t.Helper()

		return batch(operation, input)[key].([]any)
	}
	jobs := func(queue, status string) []any {
		t.Helper()

		return list("ListJobs", "JobSummaryList", map[string]any{"JobQueue": queue, "JobStatus": status})
	}
	describeJob := func(id any) map[string]any {
		t.Helper()

		return list("DescribeJobs", "Jobs", map[string]any{"Jobs": []any{id}})[0].(map[string]any)
	}
	tags := func(arn any) map[string]any {
		t.Helper()

		return batch("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)
	}

	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "batch") ||
		caps["batch_runtime"] != "records" {
		t.Fatal(caps)
	}

	vpc := call("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.40.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnet := call("ec2", "CreateSubnet", map[string]any{
		"VpcId": vpc, "CidrBlock": "10.40.0.0/24", "AvailabilityZone": "eu-west-1a",
	})["Subnet"].(map[string]any)["SubnetId"]
	group := call("ec2", "CreateSecurityGroup", map[string]any{
		"VpcId": vpc, "GroupName": "batch", "Description": "Batch console test",
	})["GroupId"]

	// Compute environments: managed EC2 capacity and an unmanaged environment.
	ce := batch("CreateComputeEnvironment", map[string]any{
		"ComputeEnvironmentName": "console-ce",
		"Type":                   "MANAGED",
		"State":                  "ENABLED",
		"ComputeResources": map[string]any{
			"Type":               "EC2",
			"AllocationStrategy": "BEST_FIT_PROGRESSIVE",
			"MinvCpus":           0,
			"DesiredvCpus":       2,
			"MaxvCpus":           8,
			"InstanceTypes":      []any{"optimal"},
			"InstanceRole":       "ecsInstanceRole",
			"Subnets":            []any{subnet},
			"SecurityGroupIds":   []any{group},
		},
		"Tags": map[string]any{"env": "test"},
	})["ComputeEnvironmentArn"]
	batch("CreateComputeEnvironment", map[string]any{
		"ComputeEnvironmentName": "console-unmanaged",
		"Type":                   "UNMANAGED",
		"State":                  "ENABLED",
		"ServiceRole":            "arn:aws:iam::" + account + ":role/batch",
	})
	environments := list("DescribeComputeEnvironments", "ComputeEnvironments", map[string]any{"MaxResults": 100})
	if len(environments) != 2 {
		t.Fatal(environments)
	}

	batch("UpdateComputeEnvironment", map[string]any{
		"ComputeEnvironment": "console-ce",
		"ComputeResources":   map[string]any{"MinvCpus": 1, "DesiredvCpus": 4, "MaxvCpus": 16},
	})
	managed := list("DescribeComputeEnvironments", "ComputeEnvironments", map[string]any{
		"ComputeEnvironments": []any{"console-ce"},
	})[0].(map[string]any)
	resources := managed["ComputeResources"].(map[string]any)
	if managed["State"] != "ENABLED" || managed["Status"] != "VALID" || resources["MinvCpus"] != float64(1) ||
		resources["DesiredvCpus"] != float64(4) || resources["MaxvCpus"] != float64(16) ||
		resources["InstanceRole"] != "ecsInstanceRole" || managed["Tags"].(map[string]any)["env"] != "test" {
		t.Fatal(managed)
	}

	// Job queues order their compute environments; an update replaces the order.
	queue := batch("CreateJobQueue", map[string]any{
		"JobQueueName": "console-queue",
		"State":        "ENABLED",
		"Priority":     5,
		"ComputeEnvironmentOrder": []any{
			map[string]any{"Order": 1, "ComputeEnvironment": "console-ce"},
		},
		"Tags": map[string]any{"env": "test"},
	})["JobQueueArn"]
	batch("UpdateJobQueue", map[string]any{
		"JobQueue": "console-queue",
		"Priority": 7,
		"ComputeEnvironmentOrder": []any{
			map[string]any{"Order": 1, "ComputeEnvironment": "console-ce"},
			map[string]any{"Order": 2, "ComputeEnvironment": "console-unmanaged"},
		},
	})
	described := list("DescribeJobQueues", "JobQueues", map[string]any{"MaxResults": 100})[0].(map[string]any)
	order := described["ComputeEnvironmentOrder"].([]any)
	if described["Priority"] != float64(7) || described["Status"] != "VALID" || len(order) != 2 ||
		order[0].(map[string]any)["ComputeEnvironment"] != ce {
		t.Fatal(described)
	}

	// Definitions take the container JSON the AWS CLI uses; each registration is a new revision.
	definition := map[string]any{
		"JobDefinitionName": "console-job",
		"Type":              "container",
		"ContainerProperties": map[string]any{
			"image":   "busybox",
			"command": []any{"echo", "Ref::message"},
			"resourceRequirements": []any{
				map[string]any{"type": "VCPU", "value": "1"},
				map[string]any{"type": "MEMORY", "value": "512"},
			},
		},
		"Parameters":           map[string]any{"message": "hello"},
		"PlatformCapabilities": []any{"EC2"},
		"RetryStrategy":        map[string]any{"Attempts": 2},
		"Timeout":              map[string]any{"AttemptDurationSeconds": 60},
		"Tags":                 map[string]any{"env": "test"},
	}
	first := batch("RegisterJobDefinition", definition)
	revision := batch("RegisterJobDefinition", definition)
	if first["Revision"] != float64(1) || revision["Revision"] != float64(2) {
		t.Fatal(first, revision)
	}

	revisions := list("DescribeJobDefinitions", "JobDefinitions", map[string]any{"Status": "ACTIVE", "MaxResults": 100})
	if len(revisions) != 2 {
		t.Fatal(revisions)
	}

	// Without a runtime a submitted job waits as RUNNABLE; a dependent job waits as PENDING.
	submitted := batch("SubmitJob", map[string]any{
		"JobName":       "first",
		"JobQueue":      "console-queue",
		"JobDefinition": "console-job:1",
		"Parameters":    map[string]any{"message": "from console"},
		"ContainerOverrides": map[string]any{
			"Environment": []any{map[string]any{"Name": "STAGE", "Value": "dev"}},
			"ResourceRequirements": []any{
				map[string]any{"Type": "VCPU", "Value": "2"},
				map[string]any{"Type": "MEMORY", "Value": "1024"},
			},
		},
		"RetryStrategy": map[string]any{"Attempts": 3},
		"Timeout":       map[string]any{"AttemptDurationSeconds": 120},
		"Tags":          map[string]any{"run": "1"},
	})
	id, jobArn := submitted["JobId"], submitted["JobArn"]
	job := describeJob(id)
	container := job["Container"].(map[string]any)
	if job["Status"] != "RUNNABLE" || job["CreatedAt"] == nil || container["Command"].([]any)[1] != "from console" ||
		container["Environment"].([]any)[0].(map[string]any)["Name"] != "STAGE" ||
		container["ResourceRequirements"].([]any)[0].(map[string]any)["Value"] != "2" ||
		job["RetryStrategy"].(map[string]any)["Attempts"] != float64(3) {
		t.Fatal(job)
	}

	dependent := batch("SubmitJob", map[string]any{
		"JobName":       "dependent",
		"JobQueue":      "console-queue",
		"JobDefinition": "console-job",
		"DependsOn":     []any{map[string]any{"JobId": id}},
	})["JobId"]
	if describeJob(dependent)["Status"] != "PENDING" {
		t.Fatal(describeJob(dependent))
	}

	array := batch("SubmitJob", map[string]any{
		"JobName":         "array",
		"JobQueue":        "console-queue",
		"JobDefinition":   "console-job",
		"ArrayProperties": map[string]any{"Size": 3},
		"DependsOn":       []any{map[string]any{"Type": "SEQUENTIAL"}},
	})["JobId"]
	children := list("ListJobs", "JobSummaryList", map[string]any{"ArrayJobId": array})
	if len(children) != 3 || describeJob(array)["ArrayProperties"].(map[string]any)["Size"] != float64(3) {
		t.Fatal(children)
	}

	// A status lists one state of the queue; a name filter of * lists every state.
	if len(jobs("console-queue", "RUNNABLE")) != 1 || len(jobs("console-queue", "PENDING")) != 2 ||
		len(jobs("console-queue", "FAILED")) != 0 {
		t.Fatal(jobs("console-queue", "RUNNABLE"), jobs("console-queue", "PENDING"))
	}

	all := list("ListJobs", "JobSummaryList", map[string]any{
		"JobQueue": "console-queue",
		"Filters":  []any{map[string]any{"Name": "JOB_NAME", "Values": []any{"*"}}},
	})
	if len(all) != 3 {
		t.Fatal(all)
	}

	// Cancelling the RUNNABLE job fails it, and the job that depends on it.
	batch("CancelJob", map[string]any{"JobId": id, "Reason": "Cancelled from the console"})
	job = describeJob(id)
	if job["Status"] != "FAILED" || job["StatusReason"] != "Cancelled from the console" || job["StoppedAt"] == nil {
		t.Fatal(job)
	}

	if blocked := describeJob(dependent); blocked["Status"] != "FAILED" {
		t.Fatal(blocked)
	}

	batch("TerminateJob", map[string]any{"JobId": array, "Reason": "Stopped"})
	if job = describeJob(array); job["Status"] != "FAILED" || job["StatusReason"] != "Stopped" {
		t.Fatal(job)
	}

	// Every kind of resource keeps tags under its own ARN.
	arns := map[string]any{
		"environment": ce,
		"queue":       queue,
		"definition":  first["JobDefinitionArn"],
		"job":         jobArn,
	}
	for kind, arn := range arns {
		batch("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"team": "core"}})
		batch("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"env", "run"}})

		if got := tags(arn); got["team"] != "core" || got["env"] != nil || got["run"] != nil {
			t.Fatal(kind, got)
		}
	}

	// Deregistering keeps the revision as INACTIVE.
	batch("DeregisterJobDefinition", map[string]any{"JobDefinition": first["JobDefinitionArn"]})
	inactive := list("DescribeJobDefinitions", "JobDefinitions", map[string]any{"Status": "INACTIVE"})
	if len(inactive) != 1 || inactive[0].(map[string]any)["Revision"] != float64(1) {
		t.Fatal(inactive)
	}

	// Queues and environments must be disabled first, and an environment must leave its queues.
	denied := failure("DeleteJobQueue", map[string]any{"JobQueue": "console-queue"})
	if denied["code"] != "ClientException" {
		t.Fatal(denied)
	}

	batch("UpdateJobQueue", map[string]any{"JobQueue": "console-queue", "State": "DISABLED"})
	batch("UpdateComputeEnvironment", map[string]any{"ComputeEnvironment": "console-ce", "State": "DISABLED"})

	denied = failure("DeleteComputeEnvironment", map[string]any{"ComputeEnvironment": "console-ce"})
	if denied["code"] != "ClientException" {
		t.Fatal(denied)
	}

	batch("DeleteJobQueue", map[string]any{"JobQueue": "console-queue"})
	batch("DeleteComputeEnvironment", map[string]any{"ComputeEnvironment": "console-ce"})
	if len(list("DescribeJobQueues", "JobQueues", map[string]any{})) != 0 ||
		len(list("DescribeComputeEnvironments", "ComputeEnvironments", map[string]any{})) != 1 {
		t.Fatal("resources retained")
	}

	// Operations outside the page's allowlist stay unavailable.
	denied = failure("DescribeSchedulingPolicies", map[string]any{})
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}
}
