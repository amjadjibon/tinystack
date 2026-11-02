export type ExpressionKind = "rate" | "cron" | "at";
export type RateUnit = "minute" | "hour" | "day";

export type ScheduleForm = {
	name: string;
	group: string;
	description: string;
	kind: ExpressionKind;
	rateValue: string;
	rateUnit: RateUnit;
	cron: string;
	at: string;
	timezone: string;
	start: string;
	end: string;
	window: "OFF" | "FLEXIBLE";
	windowMinutes: string;
	state: "ENABLED" | "DISABLED";
	after: "NONE" | "DELETE";
	kmsKeyArn: string;
	targetArn: string;
	roleArn: string;
	input: string;
	retryAge: string;
	retryAttempts: string;
	dlqArn: string;
	detailType: string;
	source: string;
	partitionKey: string;
	messageGroupId: string;
};

export type ScheduleTarget = {
	Arn?: string;
	RoleArn?: string;
	Input?: string;
	DeadLetterConfig?: { Arn?: string };
	RetryPolicy?: {
		MaximumEventAgeInSeconds?: number;
		MaximumRetryAttempts?: number;
	};
	EventBridgeParameters?: { DetailType?: string; Source?: string };
	KinesisParameters?: { PartitionKey?: string };
	SqsParameters?: { MessageGroupId?: string };
};

export type ScheduleDetail = {
	Arn?: string;
	Name?: string;
	GroupName?: string;
	ScheduleExpression?: string;
	ScheduleExpressionTimezone?: string;
	StartDate?: number;
	EndDate?: number;
	Description?: string;
	State?: string;
	ActionAfterCompletion?: string;
	KmsKeyArn?: string;
	CreationDate?: number;
	LastModificationDate?: number;
	FlexibleTimeWindow?: { Mode?: string; MaximumWindowInMinutes?: number };
	Target?: ScheduleTarget;
};

export const defaultGroup = "default";
export const universalPrefix = "arn:aws:scheduler:::aws-sdk:";

export const expressionHints: Record<
	ExpressionKind,
	{ label: string; placeholder: string; description: string }
> = {
	rate: {
		label: "Rate-based schedule",
		placeholder: "rate(5 minutes)",
		description: "Runs at a regular interval, such as every 5 minutes.",
	},
	cron: {
		label: "Cron-based schedule",
		placeholder: "0 12 * * ? *",
		description:
			"Six fields: minutes hours day-of-month month day-of-week year. Use ? in one of the two day fields.",
	},
	at: {
		label: "One-time schedule",
		placeholder: "2030-01-31T09:30:00",
		description:
			"Runs once at a date and time in the schedule's time zone, written as YYYY-MM-DDThh:mm:ss.",
	},
};

export function emptyScheduleForm(group = defaultGroup): ScheduleForm {
	return {
		name: "",
		group,
		description: "",
		kind: "rate",
		rateValue: "5",
		rateUnit: "minute",
		cron: "",
		at: "",
		timezone: "UTC",
		start: "",
		end: "",
		window: "OFF",
		windowMinutes: "",
		state: "ENABLED",
		after: "NONE",
		kmsKeyArn: "",
		targetArn: "",
		roleArn: "",
		input: "",
		retryAge: "",
		retryAttempts: "",
		dlqArn: "",
		detailType: "",
		source: "",
		partitionKey: "",
		messageGroupId: "",
	};
}

export function rateExpression(value: string, unit: RateUnit) {
	if (!/^[1-9]\d*$/.test(value) || !Number.isSafeInteger(Number(value)))
		throw new Error("The rate must be a whole number of 1 or more.");
	return `rate(${value} ${unit}${value === "1" ? "" : "s"})`;
}

export function atExpression(value: string) {
	const text = value.trim();
	if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}$/.test(text))
		throw new Error("The one-time date must look like 2030-01-31T09:30:00.");
	if (Number.isNaN(Date.parse(`${text}Z`)))
		throw new Error("The one-time date is not a real date and time.");
	return `at(${text})`;
}

export function cronExpression(value: string) {
	const text = value
		.trim()
		.replace(/^cron\(/, "")
		.replace(/\)$/, "")
		.trim();
	if (text.split(/\s+/).length !== 6)
		throw new Error(
			"A cron expression has six fields: minutes hours day-of-month month day-of-week year.",
		);
	return `cron(${text.split(/\s+/).join(" ")})`;
}

export function scheduleExpression(form: ScheduleForm) {
	if (form.kind === "rate")
		return rateExpression(form.rateValue.trim(), form.rateUnit);
	if (form.kind === "cron") return cronExpression(form.cron);
	return atExpression(form.at);
}

// splitExpression fills the expression fields from a stored expression.
export function splitExpression(
	expression: string,
): Pick<ScheduleForm, "kind" | "rateValue" | "rateUnit" | "cron" | "at"> {
	const base = {
		rateValue: "5",
		rateUnit: "minute" as RateUnit,
		cron: "",
		at: "",
	};
	const rate = /^rate\((\d+) (minute|hour|day)s?\)$/.exec(expression);
	if (rate)
		return {
			...base,
			kind: "rate",
			rateValue: rate[1],
			rateUnit: rate[2] as RateUnit,
		};
	const cron = /^cron\((.*)\)$/.exec(expression);
	if (cron) return { ...base, kind: "cron", cron: cron[1] };
	const at = /^at\((.*)\)$/.exec(expression);
	if (at) return { ...base, kind: "at", at: at[1] };
	return { ...base, kind: "rate" };
}

export function validName(name: string, label: string) {
	if (!/^[0-9a-zA-Z\-_.]{1,64}$/.test(name))
		throw new Error(
			`The ${label} name must be 1 to 64 letters, digits, and -_. characters.`,
		);
	return name;
}

export function validTimezone(zone: string) {
	try {
		new Intl.DateTimeFormat("en-US", { timeZone: zone });
		return true;
	} catch {
		return false;
	}
}

// parseDate turns an ISO 8601 timestamp such as 2030-01-31T09:30:00Z into epoch
// seconds. A timestamp without an offset is read as UTC.
export function parseDate(text: string, label: string): number | undefined {
	const value = text.trim();
	if (!value) return undefined;
	if (
		!/^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2})?(Z|[+-]\d{2}:\d{2})?)?$/.test(
			value,
		)
	)
		throw new Error(`${label} must look like 2030-01-31T09:30:00Z.`);
	const hasOffset = /(Z|[+-]\d{2}:\d{2})$/.test(value);
	const ms = Date.parse(
		hasOffset || !value.includes("T") ? value : `${value}Z`,
	);
	if (Number.isNaN(ms))
		throw new Error(`${label} is not a real date and time.`);
	return Math.floor(ms / 1000);
}

export function formatDate(seconds: number | undefined) {
	if (seconds === undefined) return "";
	return new Date(seconds * 1000).toISOString().replace(/\.\d+Z$/, "Z");
}

function whole(text: string, label: string, min: number, max: number) {
	if (!/^\d+$/.test(text) || Number(text) < min || Number(text) > max)
		throw new Error(`${label} must be a whole number from ${min} to ${max}.`);
	return Number(text);
}

export function targetService(arn: string) {
	if (arn.startsWith(universalPrefix)) return "universal";
	return /^arn:aws:([a-z0-9-]+):/.exec(arn)?.[1] ?? "";
}

export function targetInput(form: ScheduleForm): ScheduleTarget {
	const arn = form.targetArn.trim(),
		role = form.roleArn.trim();
	if (!arn) throw new Error("A target ARN is required.");
	if (!role) throw new Error("A role ARN is required.");
	const service = targetService(arn);
	if (
		![
			"lambda",
			"sqs",
			"sns",
			"events",
			"states",
			"kinesis",
			"universal",
		].includes(service)
	)
		throw new Error(
			"The target must be a Lambda function, SQS queue, SNS topic, EventBridge bus, Step Functions state machine, Kinesis stream, or a universal target (arn:aws:scheduler:::aws-sdk:<service>:<action>).",
		);
	if (
		service === "universal" &&
		arn.slice(universalPrefix.length).split(":").length !== 2
	)
		throw new Error(
			`A universal target looks like ${universalPrefix}<service>:<apiAction>.`,
		);
	const text = form.input.trim();
	if (text) {
		if (text.length > 8192)
			throw new Error("The target input must be at most 8192 characters.");
		try {
			JSON.parse(text);
		} catch {
			throw new Error("The target input must be valid JSON.");
		}
	} else if (service === "universal") {
		throw new Error("A universal target needs a JSON input.");
	}
	const target: ScheduleTarget = { Arn: arn, RoleArn: role };
	if (text) target.Input = text;
	const dlq = form.dlqArn.trim();
	if (dlq) {
		if (targetService(dlq) !== "sqs")
			throw new Error("The dead-letter queue must be an SQS queue ARN.");
		target.DeadLetterConfig = { Arn: dlq };
	}
	const age = form.retryAge.trim(),
		attempts = form.retryAttempts.trim();
	if (age || attempts) {
		target.RetryPolicy = {};
		if (age)
			target.RetryPolicy.MaximumEventAgeInSeconds = whole(
				age,
				"Maximum event age",
				60,
				86400,
			);
		if (attempts)
			target.RetryPolicy.MaximumRetryAttempts = whole(
				attempts,
				"Maximum retry attempts",
				0,
				185,
			);
	}
	if (service === "events") {
		if (!form.detailType.trim() || !form.source.trim())
			throw new Error(
				"An EventBridge target needs a detail type and a source.",
			);
		target.EventBridgeParameters = {
			DetailType: form.detailType.trim(),
			Source: form.source.trim(),
		};
	}
	if (service === "kinesis") {
		if (!form.partitionKey.trim())
			throw new Error("A Kinesis target needs a partition key.");
		target.KinesisParameters = { PartitionKey: form.partitionKey.trim() };
	}
	if (service === "sqs" && form.messageGroupId.trim())
		target.SqsParameters = { MessageGroupId: form.messageGroupId.trim() };
	return target;
}

// scheduleInput builds the CreateSchedule or UpdateSchedule input. Both take
// the full definition, so an edit sends every member again.
export function scheduleInput(form: ScheduleForm) {
	const input: Record<string, unknown> = {
		Name: validName(form.name.trim(), "schedule"),
		GroupName: validName(form.group.trim() || defaultGroup, "group"),
		ScheduleExpression: scheduleExpression(form),
		State: form.state,
		ActionAfterCompletion: form.after,
		Target: targetInput(form),
	};
	const zone = form.timezone.trim() || "UTC";
	if (!validTimezone(zone))
		throw new Error(`${zone} is not a known IANA time zone.`);
	input.ScheduleExpressionTimezone = zone;
	if (form.window === "FLEXIBLE")
		input.FlexibleTimeWindow = {
			Mode: "FLEXIBLE",
			MaximumWindowInMinutes: whole(
				form.windowMinutes.trim(),
				"The maximum flexible window",
				1,
				1440,
			),
		};
	else input.FlexibleTimeWindow = { Mode: "OFF" };
	const start = parseDate(form.start, "The start date"),
		end = parseDate(form.end, "The end date");
	if (start !== undefined) input.StartDate = start;
	if (end !== undefined) input.EndDate = end;
	if (start !== undefined && end !== undefined && start >= end)
		throw new Error("The start date must be before the end date.");
	if (form.description.trim()) {
		if (form.description.length > 512)
			throw new Error("The description must be at most 512 characters.");
		input.Description = form.description.trim();
	}
	if (form.kmsKeyArn.trim()) input.KmsKeyArn = form.kmsKeyArn.trim();
	return input;
}

// formFromSchedule prefills the edit form from GetSchedule.
export function formFromSchedule(s: ScheduleDetail): ScheduleForm {
	const t = s.Target ?? {};
	return {
		...emptyScheduleForm(s.GroupName),
		...splitExpression(s.ScheduleExpression ?? ""),
		name: s.Name ?? "",
		description: s.Description ?? "",
		timezone: s.ScheduleExpressionTimezone ?? "UTC",
		start: formatDate(s.StartDate),
		end: formatDate(s.EndDate),
		window: s.FlexibleTimeWindow?.Mode === "FLEXIBLE" ? "FLEXIBLE" : "OFF",
		windowMinutes: String(s.FlexibleTimeWindow?.MaximumWindowInMinutes ?? ""),
		state: s.State === "DISABLED" ? "DISABLED" : "ENABLED",
		after: s.ActionAfterCompletion === "DELETE" ? "DELETE" : "NONE",
		kmsKeyArn: s.KmsKeyArn ?? "",
		targetArn: t.Arn ?? "",
		roleArn: t.RoleArn ?? "",
		input: t.Input ?? "",
		retryAge: String(t.RetryPolicy?.MaximumEventAgeInSeconds ?? ""),
		retryAttempts: String(t.RetryPolicy?.MaximumRetryAttempts ?? ""),
		dlqArn: t.DeadLetterConfig?.Arn ?? "",
		detailType: t.EventBridgeParameters?.DetailType ?? "",
		source: t.EventBridgeParameters?.Source ?? "",
		partitionKey: t.KinesisParameters?.PartitionKey ?? "",
		messageGroupId: t.SqsParameters?.MessageGroupId ?? "",
	};
}

export type ArnResource = {
	service: string;
	type: string;
	account: string;
	region: string;
	name: string;
	arn: string;
};

export type ArnOption = { value: string; label: string; description: string };

const targetKinds: Record<string, string> = {
	lambda: "Lambda function",
	sqs: "SQS queue",
	sns: "SNS topic",
	states: "Step Functions state machine",
	kinesis: "Kinesis stream",
	events: "EventBridge bus",
};

// targetOptions lists the resources in the account and Region a schedule can
// invoke, and the IAM roles it can assume, from the console's resource listing.
export function targetOptions(
	resources: ArnResource[],
	scope: { account: string; region: string },
) {
	const local = resources.filter(
		(r) =>
			r.account === scope.account &&
			r.arn.startsWith("arn:aws:") &&
			(r.region === scope.region || r.arn.split(":")[3] === ""),
	);
	const options = (kind: (r: ArnResource) => string | undefined) =>
		local
			.flatMap((r) => {
				const description = kind(r);
				return description
					? [{ value: r.arn, label: r.name, description }]
					: [];
			})
			.sort((a, b) => a.value.localeCompare(b.value));
	return {
		targets: options((r) => {
			const service = targetService(r.arn);
			if (service === "events" && !r.arn.includes(":event-bus/")) return;
			if (service === "lambda" && !r.arn.includes(":function:")) return;
			return targetKinds[service];
		}),
		queues: options((r) =>
			targetService(r.arn) === "sqs" ? targetKinds.sqs : undefined,
		),
		roles: options((r) =>
			targetService(r.arn) === "iam" && r.arn.includes(":role/")
				? "IAM role"
				: undefined,
		),
	};
}
