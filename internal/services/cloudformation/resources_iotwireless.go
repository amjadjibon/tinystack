package cloudformation

import "context"

const (
	wirelessService     = "iotwireless"
	wirelessDestination = "Destination"
	wirelessRadio       = "LoRaWAN"
	wirelessStartTime   = "StartTime"
	wirelessFuota       = "FuotaTask"
)

func wirelessTagger(arn any) *tagger {
	return &tagger{
		service: wirelessService, tag: opTagResource, untag: opUntagResource,
		key: memberKey, value: memberValue,
		target: map[string]any{memberResourceArn: arn},
	}
}

func init() {
	for _, d := range []struct {
		kind, key         string
		fields, immutable []string
	}{
		{
			wirelessDestination, memberName,
			[]string{memberName, memberDescription, "Expression", "ExpressionType", memberRoleArn, memberTags},
			[]string{memberName},
		},
		{"DeviceProfile", "Id", []string{memberName, wirelessRadio, memberTags}, []string{memberName, wirelessRadio}},
		{"ServiceProfile", "Id", []string{memberName, wirelessRadio, memberTags}, []string{memberName, wirelessRadio}},
	} {
		attributes := []string{memberArn, "Id"}
		if d.kind == wirelessDestination {
			attributes = []string{memberArn}
		}

		register("AWS::IoTWireless::"+d.kind, &resourceType{
			properties: d.fields, attributes: attributes, replace: d.immutable,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := pick(map[string]any{}, p, d.fields...)
				out, err := c.call(ctx, wirelessService, "Create"+d.kind, params)
				if err != nil {
					return "", nil, err
				}

				id := outText(out, d.key)
				return id, map[string]any{memberArn: out[memberArn], "Id": id}, nil
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				if d.kind == wirelessDestination {
					params := pick(map[string]any{}, p, d.fields...)
					delete(params, memberTags)
					params[memberName] = r.Physical
					if !p.has(memberDescription) {
						params[memberDescription] = ""
					}

					if _, err := c.call(ctx, wirelessService, "UpdateDestination", params); err != nil {
						return nil, err
					}
				}

				if err := retag(ctx, c, r.Properties, p, wirelessTagger(r.Attributes[memberArn])); err != nil {
					return nil, err
				}

				return r.Attributes, nil
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, wirelessService, "Delete"+d.kind, map[string]any{d.key: r.Physical})
				if gone(err, "ResourceNotFoundException") {
					return nil
				}

				return err
			},
		})
	}
}

// wirelessGroupParams maps the configurable members shared by group and FUOTA templates.
// Read-only radio counters and unsupported nested members are rejected by catalog.Caller.
func wirelessGroupParams(p props, fuota bool) map[string]any {
	fields := []string{memberName, memberDescription, wirelessRadio}
	if fuota {
		fields = append(fields, "FirmwareUpdateImage", "FirmwareUpdateRole")
	}

	params := pick(map[string]any{}, p, fields...)
	if fuota {
		if lw, ok := params[wirelessRadio].(map[string]any); ok {
			radio := map[string]any{}
			for key, value := range lw {
				if key != wirelessStartTime {
					radio[key] = value
				}
			}
			params[wirelessRadio] = radio
		}
	}

	return params
}

func wirelessGroupActions(ctx context.Context, c *client, id, kind string, old, p props) error {
	for _, a := range []struct{ property, operation, field string }{
		{"AssociateWirelessDevice", "AssociateWirelessDeviceWith" + kind, "WirelessDeviceId"},
		{"DisassociateWirelessDevice", "DisassociateWirelessDeviceFrom" + kind, "WirelessDeviceId"},
		{"AssociateMulticastGroup", "AssociateMulticastGroupWithFuotaTask", "MulticastGroupId"},
		{"DisassociateMulticastGroup", "DisassociateMulticastGroupFromFuotaTask", "MulticastGroupId"},
	} {
		if p.has(a.property) && p.changed(old, a.property) {
			if _, err := c.call(
				ctx,
				wirelessService,
				a.operation,
				map[string]any{"Id": id, a.field: p[a.property]},
			); err != nil {
				return err
			}
		}
	}

	if kind == wirelessFuota {
		before, _ := old[wirelessRadio].(map[string]any)
		after, _ := p[wirelessRadio].(map[string]any)
		if props(after).has(wirelessStartTime) && props(after).changed(props(before), wirelessStartTime) {
			_, err := c.call(
				ctx,
				wirelessService,
				"StartFuotaTask",
				map[string]any{"Id": id, wirelessRadio: map[string]any{wirelessStartTime: after[wirelessStartTime]}},
			)
			return err
		}
	}

	return nil
}

func wirelessGroupAttributes(ctx context.Context, c *client, id, kind string) (map[string]any, error) {
	out, err := c.call(ctx, wirelessService, "Get"+kind, map[string]any{"Id": id})
	if err != nil {
		return nil, err
	}

	attrs := map[string]any{"Id": id, memberArn: out[memberArn], memberStatus: out[memberStatus]}
	if kind == wirelessFuota {
		attrs["FuotaTaskStatus"] = out[memberStatus]
	}

	if lw, ok := out[wirelessRadio].(map[string]any); ok {
		for _, key := range []string{wirelessStartTime, "NumberOfDevicesRequested", "NumberOfDevicesInGroup"} {
			if value, found := lw[key]; found {
				attrs["LoRaWAN."+key] = value
			}
		}
	}

	return attrs, nil
}

func init() {
	for _, kind := range []string{"MulticastGroup", wirelessFuota} {
		fuota := kind == wirelessFuota
		fields := []string{
			memberName,
			memberDescription,
			wirelessRadio,
			memberTags,
			"AssociateWirelessDevice",
			"DisassociateWirelessDevice",
		}
		if fuota {
			fields = append(
				fields,
				"FirmwareUpdateImage",
				"FirmwareUpdateRole",
				"AssociateMulticastGroup",
				"DisassociateMulticastGroup",
			)
		}

		attributes := []string{
			"Id",
			memberArn,
			memberStatus,
			"LoRaWAN.NumberOfDevicesRequested",
			"LoRaWAN.NumberOfDevicesInGroup",
		}
		if fuota {
			attributes = []string{"Id", memberArn, "FuotaTaskStatus", "LoRaWAN.StartTime"}
		}

		register("AWS::IoTWireless::"+kind, &resourceType{
			properties: fields, attributes: attributes,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := wirelessGroupParams(p, fuota)
				if p.has(memberTags) {
					params[memberTags] = p[memberTags]
				}

				out, err := c.call(ctx, wirelessService, "Create"+kind, params)
				if err != nil {
					return "", nil, err
				}

				id := outText(out, "Id")
				if actionErr := wirelessGroupActions(ctx, c, id, kind, nil, p); actionErr != nil {
					return "", nil, actionErr
				}

				attrs, err := wirelessGroupAttributes(ctx, c, id, kind)
				return id, attrs, err
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				params := wirelessGroupParams(p, fuota)
				params["Id"] = r.Physical
				for _, field := range []string{memberName, memberDescription} {
					if !p.has(field) {
						params[field] = ""
					}
				}

				changed := false
				fields := []string{
					memberName,
					memberDescription,
					wirelessRadio,
					"FirmwareUpdateImage",
					"FirmwareUpdateRole",
				}
				for _, field := range fields {
					if p.changed(r.Properties, field) {
						changed = true
					}
				}

				if changed {
					if _, err := c.call(ctx, wirelessService, "Update"+kind, params); err != nil {
						return nil, err
					}
				}

				if err := wirelessGroupActions(ctx, c, r.Physical, kind, r.Properties, p); err != nil {
					return nil, err
				}

				if err := retag(ctx, c, r.Properties, p, wirelessTagger(r.Attributes[memberArn])); err != nil {
					return nil, err
				}

				return wirelessGroupAttributes(ctx, c, r.Physical, kind)
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, wirelessService, "Delete"+kind, map[string]any{"Id": r.Physical})
				if gone(err, "ResourceNotFoundException") {
					return nil
				}

				return err
			},
		})
	}
}
