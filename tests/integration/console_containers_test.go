package integration_test

import (
	"context"
	"net/http"
	"testing"
)

func TestConsoleContainerPages(t *testing.T) {
	srv := server(t)
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation,
			"000000000000", "us-east-1", input), http.StatusOK)["output"].(map[string]any)
	}
	repo := map[string]any{"repositoryName": "console/images"}
	created := call("ecr", "CreateRepository", map[string]any{
		"repositoryName": "console/images", "imageTagMutability": "IMMUTABLE",
		"imageScanningConfiguration": map[string]any{"scanOnPush": true},
		"encryptionConfiguration":    map[string]any{"encryptionType": "AES256"},
	})["Repository"].(map[string]any)
	arn := created["RepositoryArn"].(string)
	call("ecr", "DescribeRepositories", map[string]any{})
	call("ecr", "PutImageTagMutability", map[string]any{
		"repositoryName": "console/images", "imageTagMutability": "MUTABLE",
	})
	call("ecr", "PutImageScanningConfiguration", map[string]any{
		"repositoryName":             "console/images",
		"imageScanningConfiguration": map[string]any{"scanOnPush": false},
	})
	digest := pushImage(context.Background(), t, ecrClient(srv, "000000000000", "us-east-1"),
		"console/images", "latest", "console-image")
	if len(call("ecr", "DescribeImages", repo)["ImageDetails"].([]any)) != 1 {
		t.Fatal("image missing from console")
	}

	deleted := call("ecr", "BatchDeleteImage", map[string]any{
		"repositoryName": "console/images", "imageIds": []any{map[string]any{"imageDigest": digest}},
	})
	if len(deleted["ImageIds"].([]any)) != 1 {
		t.Fatal("image was not deleted")
	}

	call("ecr", "SetRepositoryPolicy", map[string]any{
		"repositoryName": "console/images", "policyText": `{"Statement":[]}`,
	})
	call("ecr", "GetRepositoryPolicy", repo)
	call("ecr", "DeleteRepositoryPolicy", repo)
	const lifecycle = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any",` +
		`"countType":"imageCountMoreThan","countNumber":2},"action":{"type":"expire"}}]}`
	call("ecr", "PutLifecyclePolicy", map[string]any{
		"repositoryName": "console/images", "lifecyclePolicyText": lifecycle,
	})
	call("ecr", "GetLifecyclePolicy", repo)
	call("ecr", "DeleteLifecyclePolicy", repo)
	call("ecr", "TagResource", map[string]any{
		"resourceArn": arn, "tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	if len(call("ecr", "ListTagsForResource", map[string]any{"resourceArn": arn})["Tags"].([]any)) != 1 {
		t.Fatal("missing repository tag")
	}

	call("ecr", "UntagResource", map[string]any{"resourceArn": arn, "tagKeys": []string{"team"}})
	call("ecr", "DeleteRepository", map[string]any{"repositoryName": "console/images", "force": true})
	call("ecs", "CreateCluster", map[string]any{"clusterName": "console"})
	call("ecs", "ListClusters", map[string]any{})
	call("ecs", "DescribeClusters", map[string]any{"clusters": []string{"console"}})
	registered := call("ecs", "RegisterTaskDefinition", map[string]any{
		"family": "console", "containerDefinitions": []any{map[string]any{
			"name": "app", "image": "example:latest", "essential": true, "memory": 128,
		}},
	})
	definition := registered["TaskDefinition"].(map[string]any)["TaskDefinitionArn"].(string)
	call("ecs", "ListTaskDefinitions", map[string]any{})
	call("ecs", "DescribeTaskDefinition", map[string]any{"taskDefinition": definition})
	call("ecs", "CreateService", map[string]any{
		"cluster": "console", "serviceName": "app",
		"taskDefinition": definition, "desiredCount": 0,
	})
	call("ecs", "ListServices", map[string]any{"cluster": "console"})
	replacement := call("ecs", "RegisterTaskDefinition", map[string]any{
		"family": "console", "containerDefinitions": []any{map[string]any{
			"name": "app", "image": "example:v2", "essential": true, "memory": 128,
		}},
	})["TaskDefinition"].(map[string]any)["TaskDefinitionArn"].(string)
	updated := call("ecs", "UpdateService", map[string]any{
		"cluster": "console", "service": "app", "taskDefinition": replacement, "desiredCount": 1,
	})["Service"].(map[string]any)
	if updated["TaskDefinition"] != replacement || updated["DesiredCount"] != float64(1) {
		t.Fatal("service update did not retain revision and desired count", updated)
	}

	call("ecs", "DescribeServices", map[string]any{"cluster": "console", "services": []string{"app"}})
	task := call("ecs", "RunTask", map[string]any{
		"cluster":        "console",
		"taskDefinition": definition, "count": 1,
	})["Tasks"].([]any)[0].(map[string]any)
	taskARN := task["TaskArn"].(string)
	call("ecs", "ListTasks", map[string]any{"cluster": "console", "desiredStatus": "RUNNING"})
	call("ecs", "DescribeTasks", map[string]any{"cluster": "console", "tasks": []string{taskARN}})
	call("ecs", "StopTask", map[string]any{"cluster": "console", "task": taskARN})
	stopped := call("ecs", "ListTasks", map[string]any{"cluster": "console", "desiredStatus": "STOPPED"})
	if len(stopped["TaskArns"].([]any)) != 1 {
		t.Fatal("stopped task missing")
	}

	call("ecs", "DeleteService", map[string]any{"cluster": "console", "service": "app", "force": true})
	call("ecs", "DeregisterTaskDefinition", map[string]any{"taskDefinition": definition})
	call("ecs", "DeregisterTaskDefinition", map[string]any{"taskDefinition": replacement})
	call("ecs", "DeleteCluster", map[string]any{"cluster": "console"})
	for service, operation := range map[string]string{"ecr": "PutImage", "ecs": "UpdateCluster"} {
		denied := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, "000000000000", "us-east-1", map[string]any{}),
			http.StatusBadRequest)
		if denied["code"] != "UnknownOperationException" {
			t.Fatal("operation crossed the console allowlist", denied)
		}
	}
}
