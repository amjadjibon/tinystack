package cloudformation

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	propertyEFSIP          = "IpAddress"
	propertyEFSIPType      = "IpAddressType"
	propertyEFSClientToken = "ClientToken"

	serviceEFS          = "efs"
	memberFileSystemID  = "FileSystemId"
	memberMountTargetID = "MountTargetId"
	memberAccessPointID = "AccessPointId"
	propertyFSTags      = "FileSystemTags"
	propertyAPTags      = "AccessPointTags"
	propertyThroughput  = "ThroughputMode"
	propertyProvisioned = "ProvisionedThroughputInMibps"
)

func init() {
	register("AWS::EFS::FileSystem", &resourceType{
		properties: []string{
			"Encrypted",
			"KmsKeyId",
			"PerformanceMode",
			propertyThroughput,
			propertyProvisioned,
			propertyFSTags,
		},
		replace: []string{
			"Encrypted",
			"KmsKeyId",
			"PerformanceMode",
		},
		attributes: []string{memberArn, memberFileSystemID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			tags, err := efsTags(p, propertyFSTags)
			if err != nil {
				return "", nil, err
			}
			params := pick(
				map[string]any{"CreationToken": c.generate(64), memberTags: tagList(tags, memberKey, memberValue)},
				p,
				"Encrypted",
				"KmsKeyId",
				"PerformanceMode",
				propertyThroughput,
				propertyProvisioned,
			)
			out, err := c.call(ctx, serviceEFS, "CreateFileSystem", params)
			if err != nil {
				return "", nil, err
			}
			out[memberArn] = out["FileSystemArn"]
			return outText(out, memberFileSystemID), out, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := efsTags(p, propertyFSTags); err != nil {
				return nil, err
			}
			// Snapshot throughput so failure after its update restores the actual state,
			// even though the runner retains the old template on handler failure.
			before, err := efsRead(ctx, c, "DescribeFileSystems", "FileSystems", memberFileSystemID, r.Physical)
			if err != nil {
				return nil, err
			}
			changed := p.changed(props(r.Properties), propertyThroughput) ||
				p.changed(props(r.Properties), propertyProvisioned)
			if changed {
				params := efsThroughput(p, r.Physical)
				if _, err = c.call(ctx, serviceEFS, "UpdateFileSystem", params); err != nil {
					return nil, err
				}
			}
			if err = efsRetag(ctx, c, r, p, propertyFSTags); err != nil {
				if changed {
					restore := pick(
						map[string]any{memberFileSystemID: r.Physical},
						props(before),
						propertyThroughput,
						propertyProvisioned,
					)
					if _, restoreErr := c.call(
						context.WithoutCancel(ctx),
						serviceEFS,
						"UpdateFileSystem",
						restore,
					); restoreErr != nil {
						return nil, errors.Join(err, restoreErr)
					}
				}
				return nil, err
			}
			out, err := efsRead(ctx, c, "DescribeFileSystems", "FileSystems", memberFileSystemID, r.Physical)
			if err == nil {
				out[memberArn] = out["FileSystemArn"]
			}
			return out, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteEFS(ctx, c, "DeleteFileSystem", memberFileSystemID, r.Physical, "FileSystemNotFound")
		},
	})
	register("AWS::EFS::MountTarget", &resourceType{
		properties: []string{memberFileSystemID, memberSubnetID, propertyEFSIP, propertyEFSIPType, "SecurityGroups"},
		replace:    []string{memberFileSystemID, memberSubnetID, propertyEFSIP, propertyEFSIPType},
		attributes: []string{propertyEFSIP},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if !p.has("SecurityGroups") {
				return "", nil, validation("MountTarget SecurityGroups is required")
			}
			out, err := c.call(ctx, serviceEFS, "CreateMountTarget", pick(map[string]any{}, p,
				memberFileSystemID, memberSubnetID, propertyEFSIP, propertyEFSIPType, "SecurityGroups"))
			return outText(out, memberMountTargetID), out, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if !p.has("SecurityGroups") {
				return nil, validation("MountTarget SecurityGroups is required")
			}
			if _, err := c.call(ctx, serviceEFS, "ModifyMountTargetSecurityGroups",
				map[string]any{memberMountTargetID: r.Physical, "SecurityGroups": p["SecurityGroups"]}); err != nil {
				return nil, err
			}
			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteEFS(ctx, c, "DeleteMountTarget", memberMountTargetID, r.Physical, "MountTargetNotFound")
		},
	})
	register("AWS::EFS::AccessPoint", &resourceType{
		properties: []string{memberFileSystemID, propertyEFSClientToken, "PosixUser", "RootDirectory", propertyAPTags},
		replace:    []string{memberFileSystemID, propertyEFSClientToken, "PosixUser", "RootDirectory"},
		attributes: []string{memberArn, memberAccessPointID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			tags, err := efsTags(p, propertyAPTags)
			if err != nil {
				return "", nil, err
			}
			params := pick(
				map[string]any{
					propertyEFSClientToken: c.generate(64),
					memberTags:             tagList(tags, memberKey, memberValue),
				},
				p,
				memberFileSystemID,
				propertyEFSClientToken,
				"PosixUser",
				"RootDirectory",
			)
			if guardErr := efsPOSIX(params); guardErr != nil {
				return "", nil, guardErr
			}
			out, err := c.call(ctx, serviceEFS, "CreateAccessPoint", params)
			if err != nil {
				return "", nil, err
			}
			out[memberArn] = out["AccessPointArn"]
			return outText(out, memberAccessPointID), out, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := efsRetag(ctx, c, r, p, propertyAPTags); err != nil {
				return nil, err
			}
			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteEFS(ctx, c, "DeleteAccessPoint", memberAccessPointID, r.Physical, "AccessPointNotFound")
		},
	})
}

func efsThroughput(p props, id string) map[string]any {
	params := pick(
		map[string]any{memberFileSystemID: id, propertyThroughput: "bursting"},
		p,
		propertyThroughput,
		propertyProvisioned,
	)
	return params
}

func efsRead(ctx context.Context, c *client, operation, list, member, id string) (map[string]any, error) {
	out, err := c.call(ctx, serviceEFS, operation, map[string]any{member: id})
	if err != nil {
		return nil, err
	}
	items, _ := out[list].([]any)
	if len(items) != 1 {
		return nil, validation("EFS resource was not returned")
	}
	result, _ := items[0].(map[string]any)
	return result, nil
}

func deleteEFS(ctx context.Context, c *client, operation, member, id, missing string) error {
	_, err := c.call(ctx, serviceEFS, operation, map[string]any{member: id})
	if gone(err, missing) {
		return nil
	}
	return err
}

func efsTags(p props, member string) (map[string]string, error) {
	out := map[string]string{}
	if !p.has(member) {
		return out, nil
	}
	list, ok := p[member].([]any)
	if !ok {
		return nil, validation("%s must be an array of Key/Value objects", member)
	}
	for _, item := range list {
		tag, ok := item.(map[string]any)
		if !ok || len(tag) != 2 {
			return nil, validation("Tags require only Key and Value")
		}
		key, kok := tag[memberKey].(string)
		value, vok := tag[memberValue].(string)
		if !kok || !vok {
			return nil, validation("Tag Key and Value must be strings")
		}
		if _, dup := out[key]; dup {
			return nil, validation("Duplicate tag key")
		}
		out[key] = value
	}
	if err := tagging.CheckSet(out); err != nil {
		return nil, validation("%s", err.Error())
	}
	return out, nil
}

func efsRetag(ctx context.Context, c *client, r *resource, p props, member string) error {
	desired, err := efsTags(p, member)
	if err != nil {
		return err
	}
	old, err := efsTags(props(r.Properties), member)
	if err != nil {
		return err
	}
	set, removed := tagChanges(old, desired)
	target := map[string]any{"ResourceId": r.Physical}
	restore := map[string]string{}
	if len(removed) > 0 && len(set) > 0 {
		out, e := c.call(ctx, serviceEFS, "ListTagsForResource", target)
		if e != nil {
			return e
		}
		live, _ := out[memberTags].([]any)
		for _, item := range live {
			tag, _ := item.(map[string]any)
			key, _ := tag[memberKey].(string)
			for _, gone := range removed {
				if gone == key {
					restore[key] = scalarText(tag[memberValue])
				}
			}
		}
	}
	if len(removed) > 0 {
		params := maps.Clone(target)
		params["TagKeys"] = removed
		if _, err = c.call(ctx, serviceEFS, opUntagResource, params); err != nil {
			return err
		}
	}
	if len(set) > 0 {
		params := maps.Clone(target)
		params[memberTags] = tagList(set, memberKey, memberValue)
		_, err = c.call(ctx, serviceEFS, opTagResource, params)
		if err != nil && len(restore) > 0 {
			params[memberTags] = tagList(restore, memberKey, memberValue)
			if _, e := c.call(context.WithoutCancel(ctx), serviceEFS, opTagResource, params); e != nil {
				return errors.Join(err, e)
			}
		}
	}
	return err
}

// CloudFormation models POSIX IDs as strings; the API models integers. Convert
// only these fields, retaining other keys for strict generated input validation.
func efsPOSIX(params map[string]any) error {
	convert := func(value any, keys ...string) (any, error) {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, validation("POSIX configuration must be an object")
		}
		out := maps.Clone(m)
		for _, key := range keys {
			if text, ok := out[key].(string); ok {
				n, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					return nil, validation("Invalid POSIX integer %s", key)
				}
				out[key] = float64(n)
			}
		}
		return out, nil
	}
	if value, ok := params["PosixUser"]; ok {
		v, err := convert(value, "Uid", "Gid")
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if groups, ok := m["SecondaryGids"].([]any); ok {
			list := append([]any{}, groups...)
			for i, item := range list {
				if text, ok := item.(string); ok {
					n, e := strconv.ParseInt(text, 10, 64)
					if e != nil {
						return validation("Invalid SecondaryGids")
					}
					list[i] = float64(n)
				}
			}
			m["SecondaryGids"] = list
		}
		params["PosixUser"] = m
	}
	if value, ok := params["RootDirectory"]; ok {
		root, ok := value.(map[string]any)
		if !ok {
			return validation("RootDirectory must be an object")
		}
		root = maps.Clone(root)
		if ci, ok := root["CreationInfo"]; ok {
			v, e := convert(ci, "OwnerUid", "OwnerGid")
			if e != nil {
				return e
			}
			root["CreationInfo"] = v
		}
		params["RootDirectory"] = root
	}
	return nil
}

// efsRestoreReplacement reuses the exact retired incarnation on rollback.
// EFS mount targets cannot be recreated alongside the original target in its AZ,
// and explicit client tokens must not create a different access point.
func efsRestoreReplacement(st *stack, next *step) (id string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || !isEFSResource(next.typ) {
		return "", nil, false
	}
	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}
	return "", nil, false
}

func isEFSResource(typ string) bool {
	return typ == "AWS::EFS::FileSystem" || typ == "AWS::EFS::MountTarget" || typ == "AWS::EFS::AccessPoint"
}

// Retire EFS child incarnations before their replaced file system. Ordinary
// active-stack deletion already follows template dependencies; retired records
// need their original physical parent references retained for this ordering.
func efsOrderRetired(st *stack) {
	for i := 0; i < len(st.Replaced); i++ {
		parent := st.Replaced[i].Resource
		if parent.Type != "AWS::EFS::FileSystem" {
			continue
		}
		for j := i + 1; j < len(st.Replaced); j++ {
			child := st.Replaced[j].Resource
			if (child.Type == "AWS::EFS::MountTarget" || child.Type == "AWS::EFS::AccessPoint") &&
				(props(child.Properties).str(memberFileSystemID) == parent.Physical ||
					props(child.Properties).str(memberFileSystemID) == scalarText(parent.Attributes[memberArn])) {
				item := st.Replaced[j]
				copy(st.Replaced[i+1:j+1], st.Replaced[i:j])
				st.Replaced[i] = item
				i--
				break
			}
		}
	}
}
