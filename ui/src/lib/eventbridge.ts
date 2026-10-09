import { api, type Resource } from "./api";
export type EventScope = {
	account: string;
	region: string;
	bus?: string;
	rule?: string;
};
export type EventBus = {
	Name?: string;
	Arn?: string;
	Description?: string;
	Policy?: string;
	CreationTime?: number;
};
export type EventRule = {
	Name?: string;
	Arn?: string;
	EventBusName?: string;
	State?: string;
	EventPattern?: string;
	ScheduleExpression?: string;
	Description?: string;
	RoleArn?: string;
};
export type EventTarget = {
	Id: string;
	Arn: string;
	RoleArn?: string;
	Input?: string;
	InputPath?: string;
	InputTransformer?: {
		InputPathsMap?: Record<string, string>;
		InputTemplate?: string;
	};
	RetryPolicy?: {
		MaximumRetryAttempts?: number;
		MaximumEventAgeInSeconds?: number;
	};
	DeadLetterConfig?: { Arn?: string };
	SqsParameters?: { MessageGroupId?: string };
};
export function eventHref(scope: EventScope, section = "buses") {
	const p = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.bus) p.set("bus", scope.bus);
	if (scope.rule) p.set("rule", scope.rule);
	if (section !== "buses") p.set("section", section);
	return `/services/eventbridge?${p}`;
}
export function eventCall<T = Record<string, unknown>>(
	scope: EventScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(
		"eventbridge",
		operation,
		scope.account,
		input,
		scope.region,
	);
}
export function eventBusArn(scope: EventScope) {
	return `arn:aws:events:${scope.region}:${scope.account}:event-bus/${scope.bus || "default"}`;
}
export function eventRuleArn(scope: EventScope) {
	return `arn:aws:events:${scope.region}:${scope.account}:rule/${scope.bus && scope.bus !== "default" ? `${scope.bus}/` : ""}${scope.rule}`;
}
export function inventoryRule(resource?: Resource): EventRule {
	return (resource?.details?.Rule as EventRule) ?? {};
}
export function inventoryTargets(resource?: Resource): EventTarget[] {
	return (
		(resource?.details?.Targets ?? []) as {
			ID: string;
			ARN: string;
			Input?: string;
			InputPath?: string;
			RoleARN?: string;
			MessageGroupID?: string;
			DeadLetterARN?: string;
			MaxRetries?: number;
			MaxAge?: number;
			Transformer?: { Paths: Record<string, string>; Template: string };
		}[]
	).map((t) => ({
		Id: t.ID,
		Arn: t.ARN,
		...(t.RoleARN ? { RoleArn: t.RoleARN } : {}),
		...(t.Input ? { Input: t.Input } : {}),
		...(t.InputPath ? { InputPath: t.InputPath } : {}),
		...(t.MessageGroupID
			? { SqsParameters: { MessageGroupId: t.MessageGroupID } }
			: {}),
		...(t.DeadLetterARN ? { DeadLetterConfig: { Arn: t.DeadLetterARN } } : {}),
		...(t.MaxRetries != null || t.MaxAge != null
			? {
					RetryPolicy: {
						MaximumRetryAttempts: t.MaxRetries,
						MaximumEventAgeInSeconds: t.MaxAge,
					},
				}
			: {}),
		...(t.Transformer
			? {
					InputTransformer: {
						InputPathsMap: t.Transformer.Paths,
						InputTemplate: t.Transformer.Template,
					},
				}
			: {}),
	}));
}
export function checkEntries(result: {
	FailedEntryCount?: number;
	FailedEntries?: { ErrorCode?: string; ErrorMessage?: string }[];
}) {
	if (result.FailedEntryCount)
		throw new Error(
			(result.FailedEntries ?? [])
				.map((e) => `${e.ErrorCode}: ${e.ErrorMessage}`)
				.join("; ") || `${result.FailedEntryCount} entries failed.`,
		);
}
