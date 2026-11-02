// Actions, conditions and rule priorities for listeners and rules.

import { isSecureProtocol, lines, parseInteger } from "./elbv2Helpers.ts";

export type Action = {
	Type?: string;
	Order?: number;
	TargetGroupArn?: string;
	ForwardConfig?: {
		TargetGroups?: { TargetGroupArn?: string; Weight?: number }[];
	};
	RedirectConfig?: {
		Protocol?: string;
		Port?: string;
		Host?: string;
		Path?: string;
		Query?: string;
		StatusCode?: string;
	};
	FixedResponseConfig?: {
		StatusCode?: string;
		ContentType?: string;
		MessageBody?: string;
	};
};

export type Condition = {
	Field?: string;
	Values?: string[];
	HostHeaderConfig?: { Values?: string[] };
	PathPatternConfig?: { Values?: string[] };
	HttpHeaderConfig?: { HttpHeaderName?: string; Values?: string[] };
	HttpRequestMethodConfig?: { Values?: string[] };
	QueryStringConfig?: { Values?: { Key?: string; Value?: string }[] };
	SourceIpConfig?: { Values?: string[] };
};

export const conditionFields = [
	{ value: "path-pattern", label: "Path pattern" },
	{ value: "host-header", label: "Host header" },
	{ value: "http-header", label: "HTTP header" },
	{ value: "http-request-method", label: "HTTP request method" },
	{ value: "query-string", label: "Query string" },
	{ value: "source-ip", label: "Source IP" },
];

export const actionTypes = [
	{ value: "forward", label: "Forward to target group" },
	{ value: "fixed-response", label: "Return fixed response" },
	{ value: "redirect", label: "Redirect" },
];

export const redirectCodes = ["HTTP_301", "HTTP_302"];
export const fixedContentTypes = [
	"text/plain",
	"text/css",
	"text/html",
	"application/javascript",
	"application/json",
];
export type ActionForm = {
	type: string;
	targetGroup: string;
	redirectCode: string;
	redirectProtocol: string;
	redirectPort: string;
	redirectHost: string;
	redirectPath: string;
	redirectQuery: string;
	fixedStatus: string;
	fixedType: string;
	fixedBody: string;
};

export const emptyActionForm: ActionForm = {
	type: "forward",
	targetGroup: "",
	redirectCode: "HTTP_301",
	redirectProtocol: "#{protocol}",
	redirectPort: "#{port}",
	redirectHost: "#{host}",
	redirectPath: "/#{path}",
	redirectQuery: "#{query}",
	fixedStatus: "200",
	fixedType: "text/plain",
	fixedBody: "",
};

export function actionInput(f: ActionForm): Action[] {
	if (f.type === "forward") {
		if (!f.targetGroup) throw new Error("Choose a target group to forward to.");
		return [{ Type: "forward", Order: 1, TargetGroupArn: f.targetGroup }];
	}
	if (f.type === "redirect") {
		return [
			{
				Type: "redirect",
				Order: 1,
				RedirectConfig: {
					StatusCode: f.redirectCode,
					Protocol: f.redirectProtocol.trim() || "#{protocol}",
					Port: f.redirectPort.trim() || "#{port}",
					Host: f.redirectHost.trim() || "#{host}",
					Path: f.redirectPath.trim() || "/#{path}",
					Query: f.redirectQuery.trim() || "#{query}",
				},
			},
		];
	}
	if (f.type === "fixed-response") {
		if (!/^[2-5]\d\d$/.test(f.fixedStatus.trim()))
			throw new Error("The fixed response status must be 200 to 599.");
		if (f.fixedBody.length > 1024)
			throw new Error("The fixed response body is at most 1024 characters.");
		return [
			{
				Type: "fixed-response",
				Order: 1,
				FixedResponseConfig: {
					StatusCode: f.fixedStatus.trim(),
					ContentType: f.fixedType,
					...(f.fixedBody ? { MessageBody: f.fixedBody } : {}),
				},
			},
		];
	}
	throw new Error("Choose an action type.");
}

export function actionFormFrom(actions?: Action[]): ActionForm {
	const a = actions?.[0];
	const base = { ...emptyActionForm };
	if (!a) return base;
	if (a.Type === "redirect")
		return {
			...base,
			type: "redirect",
			redirectCode: a.RedirectConfig?.StatusCode ?? base.redirectCode,
			redirectProtocol: a.RedirectConfig?.Protocol ?? base.redirectProtocol,
			redirectPort: a.RedirectConfig?.Port ?? base.redirectPort,
			redirectHost: a.RedirectConfig?.Host ?? base.redirectHost,
			redirectPath: a.RedirectConfig?.Path ?? base.redirectPath,
			redirectQuery: a.RedirectConfig?.Query ?? base.redirectQuery,
		};
	if (a.Type === "fixed-response")
		return {
			...base,
			type: "fixed-response",
			fixedStatus: a.FixedResponseConfig?.StatusCode ?? base.fixedStatus,
			fixedType: a.FixedResponseConfig?.ContentType ?? base.fixedType,
			fixedBody: a.FixedResponseConfig?.MessageBody ?? "",
		};
	return {
		...base,
		targetGroup:
			a.TargetGroupArn ??
			a.ForwardConfig?.TargetGroups?.[0]?.TargetGroupArn ??
			"",
	};
}

export function describeAction(a: Action, groupName: (arn: string) => string) {
	switch (a.Type) {
		case "forward": {
			const arns = a.ForwardConfig?.TargetGroups?.map(
				(t) => t.TargetGroupArn ?? "",
			) ?? [a.TargetGroupArn ?? ""];
			return `Forward to ${arns.map(groupName).join(", ")}`;
		}
		case "redirect":
			return `Redirect ${a.RedirectConfig?.StatusCode ?? ""} to ${a.RedirectConfig?.Host ?? ""}${a.RedirectConfig?.Path ?? ""}`;
		case "fixed-response":
			return `Respond ${a.FixedResponseConfig?.StatusCode ?? ""} ${a.FixedResponseConfig?.ContentType ?? ""}`.trim();
		default:
			return a.Type ?? "-";
	}
}

export type ConditionForm = {
	field: string;
	values: string;
	headerName: string;
};

export const emptyCondition: ConditionForm = {
	field: "path-pattern",
	values: "",
	headerName: "",
};

export function conditionInput(forms: ConditionForm[]): Condition[] {
	if (!forms.length) throw new Error("Add at least one condition.");
	const seen = new Set<string>();
	let total = 0;
	const out = forms.map((f) => {
		const values = lines(f.values);
		if (!values.length)
			throw new Error(`The ${f.field} condition needs at least one value.`);
		total += values.length;
		if (
			seen.has(f.field) &&
			f.field !== "http-header" &&
			f.field !== "query-string"
		)
			throw new Error(`A rule can have only one ${f.field} condition.`);
		seen.add(f.field);
		switch (f.field) {
			case "path-pattern":
				return { Field: f.field, PathPatternConfig: { Values: values } };
			case "host-header":
				return { Field: f.field, HostHeaderConfig: { Values: values } };
			case "http-header":
				if (!f.headerName.trim())
					throw new Error("The HTTP header condition needs a header name.");
				return {
					Field: f.field,
					HttpHeaderConfig: {
						HttpHeaderName: f.headerName.trim(),
						Values: values,
					},
				};
			case "http-request-method":
				return {
					Field: f.field,
					HttpRequestMethodConfig: {
						Values: values.map((v) => v.toUpperCase()),
					},
				};
			case "query-string":
				return {
					Field: f.field,
					QueryStringConfig: {
						Values: values.map((v) => {
							const at = v.indexOf("=");
							return at < 0
								? { Value: v }
								: { Key: v.slice(0, at), Value: v.slice(at + 1) };
						}),
					},
				};
			case "source-ip":
				return { Field: f.field, SourceIpConfig: { Values: values } };
			default:
				throw new Error(`Condition field ${f.field} is not supported.`);
		}
	});
	if (total > 5) throw new Error("A rule can have at most 5 condition values.");
	return out;
}

export function conditionValues(c: Condition): string[] {
	switch (c.Field) {
		case "path-pattern":
			return c.PathPatternConfig?.Values ?? c.Values ?? [];
		case "host-header":
			return c.HostHeaderConfig?.Values ?? c.Values ?? [];
		case "http-header":
			return c.HttpHeaderConfig?.Values ?? [];
		case "http-request-method":
			return c.HttpRequestMethodConfig?.Values ?? [];
		case "query-string":
			return (c.QueryStringConfig?.Values ?? []).map((p) =>
				p.Key ? `${p.Key}=${p.Value ?? ""}` : (p.Value ?? ""),
			);
		case "source-ip":
			return c.SourceIpConfig?.Values ?? [];
		default:
			return c.Values ?? [];
	}
}

export function conditionForms(conditions?: Condition[]): ConditionForm[] {
	if (!conditions?.length) return [{ ...emptyCondition }];
	return conditions.map((c) => ({
		field: c.Field ?? "path-pattern",
		values: conditionValues(c).join("\n"),
		headerName: c.HttpHeaderConfig?.HttpHeaderName ?? "",
	}));
}

export function describeCondition(c: Condition) {
	const name = c.HttpHeaderConfig?.HttpHeaderName;
	return `${c.Field}${name ? ` ${name}` : ""}: ${conditionValues(c).join(", ")}`;
}

export function rulePriority(text: string) {
	return parseInteger(text, "Priority", 1, 50000);
}

// rulePrioritiesInput lists the rules whose priority changed and rejects duplicates.
export function rulePrioritiesInput(
	rules: { RuleArn: string; Priority?: string; IsDefault?: boolean }[],
	edited: Record<string, string>,
) {
	const editable = rules.filter((r) => !r.IsDefault);
	const final = new Map<number, string>();
	const changed: { RuleArn: string; Priority: number }[] = [];
	for (const r of editable) {
		const priority = rulePriority(edited[r.RuleArn] ?? r.Priority ?? "");
		if (final.has(priority))
			throw new Error(`Priority ${priority} is used by more than one rule.`);
		final.set(priority, r.RuleArn);
		if (String(priority) !== r.Priority)
			changed.push({ RuleArn: r.RuleArn, Priority: priority });
	}
	if (!changed.length) throw new Error("No rule priority was changed.");
	return { RulePriorities: changed };
}

// nextPriority suggests the first free multiple of 10.
export function nextPriority(rules: { Priority?: string }[]) {
	const used = new Set(rules.map((r) => Number(r.Priority)));
	let n = 10;
	while (used.has(n)) n += 10;
	return n;
}

export type ListenerForm = {
	protocol: string;
	port: string;
	certificate: string;
	sslPolicy: string;
	action: ActionForm;
};

// listenerInput builds the shared members of CreateListener and ModifyListener.
export function listenerInput(f: ListenerForm) {
	if (!f.protocol) throw new Error("Choose a listener protocol.");
	const port = parseInteger(f.port, "Port", 1, 65535);
	const secure = isSecureProtocol(f.protocol);
	if (secure && !f.certificate.trim())
		throw new Error(`${f.protocol} listeners need a certificate ARN.`);
	if (secure && !f.sslPolicy) throw new Error("Choose an SSL policy.");
	return {
		Protocol: f.protocol,
		Port: port,
		DefaultActions: actionInput(f.action),
		...(secure
			? {
					Certificates: [{ CertificateArn: f.certificate.trim() }],
					SslPolicy: f.sslPolicy,
				}
			: {}),
	};
}

export function ruleCreateInput(
	listenerArn: string,
	priority: string,
	conditions: ConditionForm[],
	action: ActionForm,
) {
	return {
		ListenerArn: listenerArn,
		Priority: rulePriority(priority),
		Conditions: conditionInput(conditions),
		Actions: actionInput(action),
	};
}

export function ruleModifyInput(
	ruleArn: string,
	conditions: ConditionForm[],
	action: ActionForm,
	originalActions: Action[],
) {
	return {
		RuleArn: ruleArn,
		Conditions: conditionInput(conditions),
		// Preserve weights and other action settings that this form does not expose.
		Actions:
			JSON.stringify(action) === JSON.stringify(actionFormFrom(originalActions))
				? originalActions
				: actionInput(action),
	};
}
