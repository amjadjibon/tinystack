package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleEFSLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	send := func(service, operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation, account, region, input), status)
	}

	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return send(service, operation, input, http.StatusOK)["output"].(map[string]any)
	}

	efs := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return call("efs", operation, input)
	}

	refuse := func(operation string, input map[string]any, status int, code string) {
		t.Helper()
		if out := send("efs", operation, input, status); out["code"] != code {
			t.Fatal(operation, out)
		}
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["efs"] != true {
		t.Fatal(capabilities)
	}

	// Mount targets name EC2 subnets and security groups, which the page lists through EC2.
	vpc := call("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.20.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnet := func(cidr, zone string) string {
		t.Helper()
		out := call("ec2", "CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": cidr, "AvailabilityZone": zone})
		return out["Subnet"].(map[string]any)["SubnetId"].(string)
	}

	group := func(name string) string {
		t.Helper()
		out := call("ec2", "CreateSecurityGroup", map[string]any{"VpcId": vpc, "GroupName": name, "Description": name})
		return out["GroupId"].(string)
	}

	first, sameZone, otherZone := subnet("10.20.0.0/24", "eu-west-1a"), subnet("10.20.1.0/24", "eu-west-1a"),
		subnet("10.20.2.0/24", "eu-west-1b")
	clients, admins := group("efs-clients"), group("efs-admins")
	listed := map[string]bool{}
	for _, row := range call("ec2", "DescribeSubnets", map[string]any{})["Subnets"].([]any) {
		listed[row.(map[string]any)["SubnetId"].(string)] = true
	}

	if !listed[first] || !listed[sameZone] || !listed[otherZone] {
		t.Fatal(listed)
	}

	// File systems: a provisioned, encrypted one with a Name tag, then a second to page through.
	created := efs("CreateFileSystem", map[string]any{
		"CreationToken": "console-fs", "PerformanceMode": "generalPurpose", "ThroughputMode": "provisioned",
		"ProvisionedThroughputInMibps": 64.5, "Encrypted": true, "KmsKeyId": "alias/console",
		"Tags": []any{
			map[string]any{"Key": "Name", "Value": "shared"}, map[string]any{"Key": "team", "Value": "core"},
		},
	})
	id := created["FileSystemId"].(string)
	size := created["SizeInBytes"].(map[string]any)
	if created["LifeCycleState"] != "available" || created["Name"] != "shared" || created["Encrypted"] != true ||
		created["ThroughputMode"] != "provisioned" || created["ProvisionedThroughputInMibps"] != 64.5 ||
		created["CreationTime"].(float64) <= 0 || size["Value"] != float64(0) {
		t.Fatal(created)
	}

	refuse("CreateFileSystem", map[string]any{"CreationToken": "console-fs"}, http.StatusConflict,
		"FileSystemAlreadyExists")
	refuse("CreateFileSystem", map[string]any{
		"CreationToken": "console-max", "PerformanceMode": "maxIO",
		"ThroughputMode": "elastic",
	}, http.StatusBadRequest, "BadRequest")
	efs("CreateFileSystem", map[string]any{"CreationToken": "console-second"})

	page := efs("DescribeFileSystems", map[string]any{"MaxItems": 1})
	if len(page["FileSystems"].([]any)) != 1 || page["NextMarker"] == nil {
		t.Fatal(page)
	}

	rest := efs("DescribeFileSystems", map[string]any{"MaxItems": 1, "Marker": page["NextMarker"]})
	if len(rest["FileSystems"].([]any)) != 1 || rest["NextMarker"] != nil {
		t.Fatal(rest)
	}

	// Changing the throughput mode drops the provisioned amount.
	updated := efs("UpdateFileSystem", map[string]any{"FileSystemId": id, "ThroughputMode": "elastic"})
	if updated["ThroughputMode"] != "elastic" || updated["ProvisionedThroughputInMibps"] != nil {
		t.Fatal(updated)
	}

	described := func() map[string]any {
		t.Helper()
		list := efs("DescribeFileSystems", map[string]any{"FileSystemId": id})["FileSystems"].([]any)
		if len(list) != 1 {
			t.Fatal(list)
		}

		return list[0].(map[string]any)
	}
	tagged := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, tag := range described()["Tags"].([]any) {
			out[tag.(map[string]any)["Key"].(string)] = tag.(map[string]any)["Value"].(string)
		}

		return out
	}

	efs("TagResource", map[string]any{"ResourceId": id, "Tags": []any{
		map[string]any{"Key": "env", "Value": "dev"},
	}})
	efs("UntagResource", map[string]any{"ResourceId": id, "TagKeys": []any{"team"}})
	if tags := tagged(); len(tags) != 2 || tags["Name"] != "shared" || tags["env"] != "dev" {
		t.Fatal(tags)
	}

	// Mount targets: one per Availability Zone, with security groups that can be replaced.
	mount := efs("CreateMountTarget", map[string]any{
		"FileSystemId": id, "SubnetId": first, "IpAddress": "10.20.0.42", "SecurityGroups": []any{clients},
	})
	mountID := mount["MountTargetId"].(string)
	if mount["SubnetId"] != first || mount["VpcId"] != vpc || mount["IpAddress"] != "10.20.0.42" ||
		mount["AvailabilityZoneName"] != "eu-west-1a" || mount["LifeCycleState"] != "available" {
		t.Fatal(mount)
	}

	refuse("CreateMountTarget", map[string]any{"FileSystemId": id, "SubnetId": sameZone},
		http.StatusConflict, "MountTargetConflict")
	second := efs("CreateMountTarget", map[string]any{"FileSystemId": id, "SubnetId": otherZone})
	if second["AvailabilityZoneName"] != "eu-west-1b" {
		t.Fatal(second)
	}

	mounts := efs("DescribeMountTargets", map[string]any{"FileSystemId": id})["MountTargets"].([]any)
	if len(mounts) != 2 || described()["NumberOfMountTargets"] != float64(2) {
		t.Fatal(mounts, described())
	}

	securityGroups := func(mountTarget string) []any {
		t.Helper()
		out := efs("DescribeMountTargetSecurityGroups", map[string]any{"MountTargetId": mountTarget})
		// A mount target without groups reports none, which the console output may leave out.
		groups, _ := out["SecurityGroups"].([]any)
		return groups
	}
	if groups := securityGroups(mountID); len(groups) != 1 || groups[0] != clients {
		t.Fatal(groups)
	}

	efs("ModifyMountTargetSecurityGroups", map[string]any{
		"MountTargetId": mountID, "SecurityGroups": []any{clients, admins},
	})
	if groups := securityGroups(mountID); len(groups) != 2 {
		t.Fatal(groups)
	}

	efs("ModifyMountTargetSecurityGroups", map[string]any{"MountTargetId": mountID, "SecurityGroups": []any{}})
	if groups := securityGroups(mountID); len(groups) != 0 {
		t.Fatal(groups)
	}

	// Access points: POSIX user, a root directory with creation info, and tags.
	point := efs("CreateAccessPoint", map[string]any{
		"ClientToken": "console-ap", "FileSystemId": id,
		"PosixUser": map[string]any{"Uid": 1001, "Gid": 2002, "SecondaryGids": []any{7, 8}},
		"RootDirectory": map[string]any{"Path": "/app/data", "CreationInfo": map[string]any{
			"OwnerUid": 1001, "OwnerGid": 2002, "Permissions": "0750",
		}},
		"Tags": []any{map[string]any{"Key": "Name", "Value": "worker"}},
	})
	pointID := point["AccessPointId"].(string)
	posix, root := point["PosixUser"].(map[string]any), point["RootDirectory"].(map[string]any)
	if point["Name"] != "worker" || point["FileSystemId"] != id || posix["Uid"] != float64(1001) ||
		len(posix["SecondaryGids"].([]any)) != 2 || root["Path"] != "/app/data" ||
		root["CreationInfo"].(map[string]any)["Permissions"] != "0750" {
		t.Fatal(point)
	}

	refuse("CreateAccessPoint", map[string]any{
		"ClientToken": "console-ap", "FileSystemId": id,
		"PosixUser": map[string]any{"Uid": 1, "Gid": 1},
	}, http.StatusConflict, "AccessPointAlreadyExists")
	refuse("CreateAccessPoint", map[string]any{
		"ClientToken": "console-bad", "FileSystemId": id,
		"RootDirectory": map[string]any{"Path": "relative"},
	}, http.StatusBadRequest, "BadRequest")
	efs("TagResource", map[string]any{"ResourceId": pointID, "Tags": []any{
		map[string]any{"Key": "owner", "Value": "platform"},
	}})

	all := efs("DescribeAccessPoints", map[string]any{})["AccessPoints"].([]any)
	scoped := efs("DescribeAccessPoints", map[string]any{"FileSystemId": id})["AccessPoints"].([]any)
	if len(all) != 1 || len(scoped) != 1 || len(scoped[0].(map[string]any)["Tags"].([]any)) != 2 {
		t.Fatal(all, scoped)
	}

	// A file system with mount targets or access points cannot be deleted.
	refuse("DeleteFileSystem", map[string]any{"FileSystemId": id}, http.StatusConflict, "FileSystemInUse")
	efs("DeleteMountTarget", map[string]any{"MountTargetId": mountID})
	efs("DeleteMountTarget", map[string]any{"MountTargetId": second["MountTargetId"]})
	refuse("DeleteFileSystem", map[string]any{"FileSystemId": id}, http.StatusConflict, "FileSystemInUse")
	efs("DeleteAccessPoint", map[string]any{"AccessPointId": pointID})
	efs("DeleteFileSystem", map[string]any{"FileSystemId": id})
	refuse("DescribeFileSystems", map[string]any{"FileSystemId": id}, http.StatusNotFound, "FileSystemNotFound")
	if left := efs("DescribeFileSystems", map[string]any{})["FileSystems"].([]any); len(left) != 1 {
		t.Fatal(left)
	}

	// Implemented operations outside this page stay unreachable through console actions.
	refuse("ListTagsForResource", map[string]any{"ResourceId": id}, http.StatusBadRequest,
		"UnknownOperationException")
}
