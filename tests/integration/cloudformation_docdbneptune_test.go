package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

func clusterStackTemplate(engine, version, memberID, class, tag string, port int) string {
	namespace := "DocDB"
	if engine == "neptune" {
		namespace = "Neptune"
	}

	tags := []any{map[string]any{"Key": "version", "Value": tag}}
	clusterProps := map[string]any{
		"DBClusterIdentifier": engine + "-cluster", "EngineVersion": version,
		"Port": port, "BackupRetentionPeriod": 7, "Tags": tags, "StorageEncrypted": true, "KmsKeyId": "alias/test",
	}

	if engine == "docdb" {
		clusterProps["MasterUsername"], clusterProps["MasterUserPassword"] = "masteruser", "Password123!"
	}

	outputs := map[string]any{
		"Cluster":  map[string]any{"Value": map[string]any{"Ref": "Cluster"}},
		"Instance": map[string]any{"Value": map[string]any{"Ref": "Instance"}},
	}

	for _, attribute := range []string{"Endpoint", "ReadEndpoint", "Port", "ClusterResourceId"} {
		outputs[attribute] = map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Cluster", attribute}}}
	}

	for _, attribute := range []string{"Endpoint", "Port"} {
		outputs["Instance"+attribute] = map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []string{"Instance", attribute}},
		}
	}

	raw, _ := json.Marshal(map[string]any{"Resources": map[string]any{
		"Cluster": map[string]any{"Type": "AWS::" + namespace + "::DBCluster", "Properties": clusterProps},
		"Instance": map[string]any{"Type": "AWS::" + namespace + "::DBInstance", "Properties": map[string]any{
			"DBClusterIdentifier": map[string]any{"Ref": "Cluster"}, "DBInstanceIdentifier": memberID,
			"DBInstanceClass": class, "Tags": tags,
		}},
	}, "Outputs": outputs})
	return string(raw)
}

func TestCloudFormationDocDBNeptuneLifecycle(t *testing.T) {
	for _, engine := range []string{"docdb", "neptune"} {
		t.Run(engine, func(t *testing.T) {
			s := server(t)
			ctx := context.Background()
			cf := cfnClient(s)
			r := rdsClient(s, "test", "us-east-1")
			version, rejectedVersion, port := "5.0", "4.0", 27017
			if engine == "neptune" {
				version, rejectedVersion, port = "1.3.2.0", "1.2.1.0", 8182
			}

			name := engine + "-stack"
			_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new(name), TemplateBody: new(
				clusterStackTemplate(engine, version, engine+"-member", "db.r5.large", "one", port),
			)})
			if err != nil {
				t.Fatal(err)
			}

			created := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
			before := outputsOf(&created)
			if before["Cluster"] != engine+"-cluster" || before["Instance"] != engine+"-member" ||
				before["Endpoint"] == "" || before["ReadEndpoint"] == "" || before["ClusterResourceId"] == "" ||
				before["Endpoint"] != before["InstanceEndpoint"] || before["Port"] != before["InstancePort"] {
				t.Fatal(before)
			}

			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(
				clusterStackTemplate(engine, version, engine+"-member", "db.r5.xlarge", "two", port+1),
			)})
			if err != nil {
				t.Fatal(err)
			}

			updated := wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateComplete)
			after := outputsOf(&updated)
			if after["Cluster"] != before["Cluster"] || after["ClusterResourceId"] != before["ClusterResourceId"] ||
				after["Endpoint"] != before["Endpoint"] || after["Port"] == before["Port"] ||
				after["Port"] != after["InstancePort"] {
				t.Fatal(after)
			}

			described, err := r.DescribeDBClusters(
				ctx,
				&rds.DescribeDBClustersInput{DBClusterIdentifier: new(engine + "-cluster")},
			)
			if err != nil || len(described.DBClusters[0].TagList) != 1 ||
				*described.DBClusters[0].TagList[0].Value != "two" {
				t.Fatal(described, err)
			}

			member, err := r.DescribeDBInstances(
				ctx,
				&rds.DescribeDBInstancesInput{DBInstanceIdentifier: new(engine + "-member")},
			)
			if err != nil || *member.DBInstances[0].DBInstanceClass != "db.r5.xlarge" ||
				len(member.DBInstances[0].TagList) != 1 || *member.DBInstances[0].TagList[0].Value != "two" {
				t.Fatal(member, err)
			}

			// Changing only the parent's port refreshes the unchanged member's inherited GetAtt attributes.
			port++
			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(
				clusterStackTemplate(engine, version, engine+"-member", "db.r5.xlarge", "two", port+1),
			)})
			if err != nil {
				t.Fatal(err)
			}

			parentOnly := wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateComplete)
			parentOnlyOutputs := outputsOf(&parentOnly)
			if parentOnlyOutputs["Port"] == after["Port"] ||
				parentOnlyOutputs["InstancePort"] != parentOnlyOutputs["Port"] {
				t.Fatal(parentOnlyOutputs)
			}

			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(
				clusterStackTemplate(engine, rejectedVersion, engine+"-member", "db.r5.xlarge", "two", port+1),
			)})
			if err != nil {
				t.Fatal(err)
			}

			wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateRollbackComplete)
			described, err = r.DescribeDBClusters(
				ctx,
				&rds.DescribeDBClustersInput{DBClusterIdentifier: new(engine + "-cluster")},
			)
			if err != nil || *described.DBClusters[0].EngineVersion != version ||
				*described.DBClusters[0].Port != int32(port+1) {
				t.Fatal(described, err)
			}

			// Required property removal fails explicitly and rolls back without dropping old settings.
			removed := []string{"DBInstanceClass"}
			if engine == "docdb" {
				removed = append(removed, "MasterUserPassword")
			}

			for _, property := range removed {
				template := clusterStackTemplate(engine, version, engine+"-member", "db.r5.xlarge", "two", port+1)
				var document map[string]any
				if err = json.Unmarshal([]byte(template), &document); err != nil {
					t.Fatal(err)
				}

				resources := document["Resources"].(map[string]any)
				logical := "Instance"
				if property == "MasterUserPassword" {
					logical = "Cluster"
				}

				resource := resources[logical].(map[string]any)
				delete(resource["Properties"].(map[string]any), property)
				body, marshalErr := json.Marshal(document)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}

				_, err = cf.UpdateStack(
					ctx,
					&cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(string(body))},
				)
				if err != nil {
					t.Fatal(err)
				}

				wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateRollbackComplete)
			}

			// Immutable identifiers replace the instance while preserving the parent cluster.
			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(
				clusterStackTemplate(engine, version, engine+"-replacement", "db.r5.xlarge", "three", port+1),
			)})
			if err != nil {
				t.Fatal(err)
			}

			replaced := wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateComplete)
			outputs := outputsOf(&replaced)
			if outputs["Instance"] != engine+"-replacement" ||
				outputs["ClusterResourceId"] != before["ClusterResourceId"] {
				t.Fatal(outputs)
			}

			described, err = r.DescribeDBClusters(
				ctx,
				&rds.DescribeDBClustersInput{DBClusterIdentifier: new(engine + "-cluster")},
			)
			if err != nil || len(described.DBClusters[0].DBClusterMembers) != 1 ||
				*described.DBClusters[0].DBClusterMembers[0].DBInstanceIdentifier != engine+"-replacement" {
				t.Fatal(described, err)
			}

			// Replacing both immutable identifiers must clean up the old member before the old parent.
			newID := engine + "-new-cluster"
			template := strings.Replace(
				clusterStackTemplate(engine, version, engine+"-new-member", "db.r5.xlarge", "four", port+1),
				engine+"-cluster", newID, 1,
			)
			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(template)})
			if err != nil {
				t.Fatal(err)
			}

			parentReplaced := wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateComplete)
			newOutputs := outputsOf(&parentReplaced)
			if newOutputs["Cluster"] != newID || newOutputs["ClusterResourceId"] == before["ClusterResourceId"] {
				t.Fatal(newOutputs)
			}

			all, err := r.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{})
			if err != nil || len(all.DBClusters) != 1 || *all.DBClusters[0].DBClusterIdentifier != newID {
				t.Fatal("retired cluster leaked", all, err)
			}

			// A new parent followed by a rejected member replacement restores the existing parent incarnation.
			invalid := strings.Replace(
				clusterStackTemplate(engine, version, engine+"-invalid-member", "bad", "five", port+1),
				engine+"-cluster", engine+"-failed-cluster", 1,
			)
			_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(invalid)})
			if err != nil {
				t.Fatal(err)
			}

			rolledBack := wantStatus(ctx, t, cf, name, cfntypes.StackStatusUpdateRollbackComplete)
			if outputsOf(&rolledBack)["ClusterResourceId"] != newOutputs["ClusterResourceId"] {
				t.Fatal("rollback replaced the original cluster", outputsOf(&rolledBack))
			}

			all, err = r.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{})
			if err != nil || len(all.DBClusters) != 1 || *all.DBClusters[0].DBClusterIdentifier != newID {
				t.Fatal("failed replacement leaked", all, err)
			}

			_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(name)})
			if err != nil {
				t.Fatal(err)
			}

			wantStatus(ctx, t, cf, *created.StackId, cfntypes.StackStatusDeleteComplete)
			_, err = r.DescribeDBClusters(
				ctx,
				&rds.DescribeDBClustersInput{DBClusterIdentifier: new(newID)},
			)
			code(t, err, "DBClusterNotFoundFault")
		})
	}
}
