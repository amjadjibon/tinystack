package cloudformation

import "testing"

func TestClusterProviderReplacement(t *testing.T) {
	for _, namespace := range []string{"DocDB", "Neptune"} {
		cluster := types["AWS::"+namespace+"::DBCluster"]
		for _, property := range []string{
			memberDBClusterIdentifier, memberDBSubnetGroupName, memberStorageEncrypted, memberKmsKeyID,
		} {
			if !cluster.replaces(props{property: "before"}, props{property: "after"}) {
				t.Fatal(namespace, property)
			}
		}

		for _, property := range []string{memberPort, memberBackupRetentionPeriod, memberDeletionProtection, memberTags} {
			if cluster.replaces(props{property: "before"}, props{property: "after"}) {
				t.Fatal(namespace, property)
			}
		}

		member := types["AWS::"+namespace+"::DBInstance"]
		if !member.replaces(props{memberDBClusterIdentifier: "before"}, props{memberDBClusterIdentifier: "after"}) ||
			member.replaces(props{memberDBInstanceClass: "before"}, props{memberDBInstanceClass: "after"}) {
			t.Fatal(namespace)
		}
	}
}
