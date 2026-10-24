package cloudformation

import (
	"context"
	"time"
)

const (
	s3filesOwner         = "OwnerId"
	s3filesStatusMessage = "StatusMessage"
	s3filesCreated       = "CreationTime"
	s3filesZone          = "AvailabilityZoneId"
	s3filesUser          = "PosixUser"
	s3filesRoot          = "RootDirectory"
	s3filesPointARN      = "AccessPointArn"
	s3filesARN           = "FileSystemArn"
	s3filesPrefix        = "Prefix"

	s3filesService     = "s3files"
	s3filesFileSystem  = "FileSystemId"
	s3filesMountTarget = "MountTargetId"
	s3filesAccessPoint = "AccessPointId"
	s3filesSync        = "SynchronizationConfiguration"
	s3filesVersion     = "LatestVersionNumber"
	s3filesNotFound    = "ResourceNotFoundException"
	s3filesAvailable   = "available"
	s3filesPolicy      = "Policy"
	s3filesGroups      = "SecurityGroups"
)

// s3filesSettle polls op until done reports the resource settled, as CloudFormation waits for S3 Files.
func s3filesSettle(
	ctx context.Context,
	c *client,
	op string,
	params map[string]any,
	done func(out map[string]any, err error) (bool, error),
) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.call(ctx, s3filesService, op, params)
		if finished, doneErr := done(out, err); finished || doneErr != nil {
			return out, doneErr
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// s3filesReady waits for the resource to be available.
func s3filesReady(ctx context.Context, c *client, op string, params map[string]any) (map[string]any, error) {
	return s3filesSettle(ctx, c, op, params, func(out map[string]any, err error) (bool, error) {
		return err == nil && outText(out, memberStatus) == s3filesAvailable, err
	})
}

// s3filesGone waits for the resource to disappear; a resource that is already gone counts as deleted.
func s3filesGone(ctx context.Context, c *client, op string, params map[string]any) error {
	_, err := s3filesSettle(ctx, c, op, params, func(_ map[string]any, err error) (bool, error) {
		if gone(err, s3filesNotFound) {
			return true, nil
		}

		return false, err
	})

	return err
}

// s3filesDelete deletes a resource and waits until it has gone, so the resources it depends on can follow.
func s3filesDelete(ctx context.Context, c *client, del, get string, params map[string]any) error {
	if _, err := c.call(ctx, s3filesService, del, params); err != nil {
		if gone(err, s3filesNotFound) {
			return nil
		}

		return err
	}

	return s3filesGone(ctx, c, get, params)
}

func s3filesTagger(id string) *tagger {
	return &tagger{
		service: s3filesService, tag: opTagResource, untag: opUntagResource,
		target: map[string]any{"ResourceId": id}, key: memberKey, value: memberValue,
	}
}

// s3filesRules returns the synchronization rules of the property, without the version the service owns.
func s3filesRules(p props, fileSystem string) map[string]any {
	params := map[string]any{s3filesFileSystem: fileSystem}
	config, _ := p[s3filesSync].(map[string]any)
	for _, k := range []string{"ImportDataRules", "ExpirationDataRules"} {
		params[k] = config[k]
	}

	return params
}

// s3filesCurrentVersion returns the version of the file system's synchronization configuration, if it has one.
func s3filesCurrentVersion(ctx context.Context, c *client, fileSystem string) (map[string]any, error) {
	out, err := c.call(ctx, s3filesService, "GetSynchronizationConfiguration",
		map[string]any{s3filesFileSystem: fileSystem})
	if gone(err, s3filesNotFound) {
		return map[string]any{}, nil
	}

	return out, err
}

func s3filesFileSystemAttributes(ctx context.Context, c *client, arn string) (map[string]any, error) {
	out, err := s3filesReady(ctx, c, "GetFileSystem", map[string]any{s3filesFileSystem: arn})
	if err != nil {
		return nil, err
	}

	attrs := map[string]any{
		s3filesARN: out[s3filesARN], s3filesFileSystem: out[s3filesFileSystem], s3filesOwner: out[s3filesOwner],
		memberStatus: out[memberStatus], s3filesStatusMessage: outText(out, s3filesStatusMessage),
	}

	if stamp, ok := out[s3filesCreated].(float64); ok {
		attrs[s3filesCreated] = time.UnixMilli(int64(stamp * 1000)).UTC().Format(time.RFC3339Nano)
	}

	version, err := s3filesCurrentVersion(ctx, c, arn)
	if err != nil {
		return nil, err
	}

	if version[s3filesVersion] != nil {
		attrs[s3filesSync+"."+s3filesVersion] = version[s3filesVersion]
	}

	return attrs, nil
}

func init() {
	register("AWS::S3Files::FileSystem", &resourceType{
		properties: []string{
			"AcceptBucketWarning",
			memberBucket,
			propertyEFSClientToken,
			memberKmsKeyID,
			s3filesPrefix,
			memberRoleArn,
			s3filesSync,
			memberTags,
		},
		attributes: []string{
			s3filesCreated, s3filesARN, s3filesFileSystem, s3filesOwner, memberStatus, s3filesStatusMessage,
			s3filesSync + "." + s3filesVersion,
		},
		replace: []string{
			"AcceptBucketWarning",
			memberBucket,
			propertyEFSClientToken,
			memberKmsKeyID,
			s3filesPrefix,
			memberRoleArn,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				"AcceptBucketWarning",
				memberBucket,
				propertyEFSClientToken,
				memberKmsKeyID,
				s3filesPrefix,
				memberRoleArn,
			)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, s3filesService, "CreateFileSystem", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, s3filesARN)
			if p.has(s3filesSync) {
				if _, err = c.call(
					ctx,
					s3filesService,
					"PutSynchronizationConfiguration",
					s3filesRules(p, arn),
				); err != nil {
					return arn, nil, err
				}
			}

			attrs, err := s3filesFileSystemAttributes(ctx, c, arn)
			return arn, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, s3filesSync) {
				if !p.has(s3filesSync) {
					return nil, validation("Removing SynchronizationConfiguration is unsupported; set explicit rules")
				}

				params := s3filesRules(p, r.Physical)
				version, err := s3filesCurrentVersion(ctx, c, r.Physical)
				if err != nil {
					return nil, err
				}

				if version[s3filesVersion] != nil {
					params[s3filesVersion] = version[s3filesVersion]
				}

				if _, err = c.call(ctx, s3filesService, "PutSynchronizationConfiguration", params); err != nil {
					return nil, err
				}
			}

			if err := retag(ctx, c, r.Properties, p, s3filesTagger(r.Physical)); err != nil {
				return nil, err
			}

			return s3filesFileSystemAttributes(ctx, c, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return s3filesDelete(
				ctx,
				c,
				"DeleteFileSystem",
				"GetFileSystem",
				map[string]any{s3filesFileSystem: r.Physical},
			)
		},
	})

	register("AWS::S3Files::MountTarget", &resourceType{
		properties: []string{
			s3filesFileSystem, propertyEFSIPType, "Ipv4Address", "Ipv6Address", s3filesGroups, memberSubnetID,
		},
		attributes: []string{
			s3filesZone, s3filesMountTarget, s3filesOwner, memberStatus, s3filesStatusMessage, memberVpcID,
		},
		replace: []string{s3filesFileSystem, propertyEFSIPType, "Ipv4Address", "Ipv6Address", memberSubnetID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, s3filesFileSystem, propertyEFSIPType, "Ipv4Address", "Ipv6Address",
				s3filesGroups, memberSubnetID)
			out, err := c.call(ctx, s3filesService, "CreateMountTarget", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, s3filesMountTarget)
			attrs, err := s3filesMountTargetAttributes(ctx, c, id)
			return id, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, s3filesGroups) {
				groups := p.list(s3filesGroups)
				if groups == nil {
					groups = []any{}
				}

				params := map[string]any{s3filesMountTarget: r.Physical, s3filesGroups: groups}
				if _, err := c.call(ctx, s3filesService, "UpdateMountTarget", params); err != nil {
					return nil, err
				}
			}

			return s3filesMountTargetAttributes(ctx, c, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return s3filesDelete(
				ctx,
				c,
				"DeleteMountTarget",
				"GetMountTarget",
				map[string]any{s3filesMountTarget: r.Physical},
			)
		},
	})

	register("AWS::S3Files::AccessPoint", &resourceType{
		properties: []string{propertyEFSClientToken, s3filesFileSystem, s3filesUser, s3filesRoot, memberTags},
		attributes: []string{s3filesPointARN, s3filesAccessPoint, s3filesOwner, memberStatus},
		replace:    []string{propertyEFSClientToken, s3filesFileSystem, s3filesUser, s3filesRoot},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, propertyEFSClientToken, s3filesFileSystem, s3filesUser, s3filesRoot)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, s3filesService, "CreateAccessPoint", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, s3filesAccessPoint)
			attrs, err := s3filesAccessPointAttributes(ctx, c, id)
			return id, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := retag(ctx, c, r.Properties, p, s3filesTagger(r.Physical)); err != nil {
				return nil, err
			}

			return s3filesAccessPointAttributes(ctx, c, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return s3filesDelete(
				ctx,
				c,
				"DeleteAccessPoint",
				"GetAccessPoint",
				map[string]any{s3filesAccessPoint: r.Physical},
			)
		},
	})

	register("AWS::S3Files::FileSystemPolicy", &resourceType{
		properties: []string{s3filesFileSystem, s3filesPolicy},
		replace:    []string{s3filesFileSystem},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := p.str(s3filesFileSystem)
			_, err := c.call(ctx, s3filesService, "PutFileSystemPolicy",
				map[string]any{s3filesFileSystem: id, s3filesPolicy: p.document(s3filesPolicy)})

			return id, nil, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			_, err := c.call(ctx, s3filesService, "PutFileSystemPolicy",
				map[string]any{s3filesFileSystem: r.Physical, s3filesPolicy: p.document(s3filesPolicy)})

			return nil, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				s3filesService,
				"DeleteFileSystemPolicy",
				map[string]any{s3filesFileSystem: r.Physical},
			)
			if gone(err, s3filesNotFound) {
				return nil
			}

			return err
		},
	})
}

func s3filesMountTargetAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, err := s3filesReady(ctx, c, "GetMountTarget", map[string]any{s3filesMountTarget: id})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		s3filesZone:          out[s3filesZone],
		s3filesMountTarget:   id,
		s3filesOwner:         out[s3filesOwner],
		memberStatus:         out[memberStatus],
		s3filesStatusMessage: outText(out, s3filesStatusMessage),
		memberVpcID:          out[memberVpcID],
	}, nil
}

func s3filesAccessPointAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, err := s3filesReady(ctx, c, "GetAccessPoint", map[string]any{s3filesAccessPoint: id})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		s3filesPointARN: out[s3filesPointARN], s3filesAccessPoint: id, s3filesOwner: out[s3filesOwner],
		memberStatus: out[memberStatus],
	}, nil
}
