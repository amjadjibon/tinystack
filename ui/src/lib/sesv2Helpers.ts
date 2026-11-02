// Pure helpers for the SES v2 page. They mirror what internal/services/sesv2
// accepts so forms reject bad input before a request is sent.

export const maxRecipients = 50;
export const eventTypes = [
	"SEND",
	"REJECT",
	"BOUNCE",
	"COMPLAINT",
	"DELIVERY",
	"OPEN",
	"CLICK",
	"RENDERING_FAILURE",
	"DELIVERY_DELAY",
	"SUBSCRIPTION",
];
export const tlsPolicies = ["OPTIONAL", "REQUIRE"];

const addressPattern = /^[^\s@<>,;]+@[^\s@<>,;]+\.[^\s@<>,;]+$/;
const domainPattern =
	/^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$/;
const arnPattern = /^arn:[a-z-]+:[a-z0-9-]+:[a-z0-9-]*:\d{12}:.+/;
const configSetPattern = /^[A-Za-z0-9_-]{1,64}$/;
const templatePattern = /^[A-Za-z0-9_-]{1,64}$/;

export type Tag = { Key?: string; Value?: string };

export function tagMap(tags?: Tag[]): Record<string, string> {
	return Object.fromEntries(
		(tags ?? []).map((t) => [t.Key ?? "", t.Value ?? ""]),
	);
}

export function tagList(tags: Record<string, string>): Tag[] {
	return Object.entries(tags).map(([Key, Value]) => ({ Key, Value }));
}

export function isEmailAddress(text: string) {
	return addressPattern.test(text);
}

export function identityKind(name: string): "email" | "domain" {
	return name.includes("@") ? "email" : "domain";
}

// validateIdentity returns what is wrong with an identity name, or "".
export function validateIdentity(name: string) {
	if (!name) return "Enter an email address or a domain.";
	if (name.includes("@"))
		return isEmailAddress(name) ? "" : `${name} is not a valid email address.`;
	return domainPattern.test(name) ? "" : `${name} is not a valid domain name.`;
}

export function validateConfigSetName(name: string) {
	return configSetPattern.test(name)
		? ""
		: "Use 1 to 64 letters, digits, hyphens or underscores.";
}

export function validateTemplateName(name: string) {
	return templatePattern.test(name)
		? ""
		: "Use 1 to 64 letters, digits, hyphens or underscores.";
}

// addressList splits recipients entered separated by commas, semicolons, spaces or lines.
export function addressList(text: string): string[] {
	return text
		.split(/[\s,;]+/)
		.map((a) => a.trim())
		.filter((a) => a);
}

export function validateAddresses(
	list: string[],
	label: string,
	required: boolean,
) {
	if (required && list.length === 0)
		return `Enter at least one ${label} address.`;
	const bad = list.find((a) => !isEmailAddress(a));
	return bad ? `${bad} is not a valid ${label} address.` : "";
}

// parseTemplateData validates the JSON object a template renders with and returns it
// compact. Empty data renders with no values.
export function parseTemplateData(text: string): string {
	if (!text.trim()) return "{}";
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error("Template data must be valid JSON.");
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error("Template data must be a JSON object.");
	return JSON.stringify(value);
}

export type TemplateForm = {
	name: string;
	subject: string;
	text: string;
	html: string;
};

export const emptyTemplate: TemplateForm = {
	name: "",
	subject: "",
	text: "",
	html: "",
};

export type TemplateContent = {
	Subject?: string;
	Text?: string;
	Html?: string;
};

export function templateForm(
	name: string,
	content?: TemplateContent,
): TemplateForm {
	return {
		name,
		subject: content?.Subject ?? "",
		text: content?.Text ?? "",
		html: content?.Html ?? "",
	};
}

// templateContent builds the content, which needs a subject and at least one body.
export function templateContent(f: TemplateForm): TemplateContent {
	if (!f.subject.trim()) throw new Error("Enter a subject.");
	if (!f.text.trim() && !f.html.trim())
		throw new Error("Enter a text body, an HTML body or both.");
	return {
		Subject: f.subject,
		...(f.text.trim() ? { Text: f.text } : {}),
		...(f.html.trim() ? { Html: f.html } : {}),
	};
}

export function createTemplateInput(
	f: TemplateForm,
	tags: Record<string, string> = {},
) {
	const problem = validateTemplateName(f.name);
	if (problem) throw new Error(problem);
	return {
		TemplateName: f.name,
		TemplateContent: templateContent(f),
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

export function updateTemplateInput(f: TemplateForm) {
	return { TemplateName: f.name, TemplateContent: templateContent(f) };
}

export type SendForm = {
	from: string;
	to: string;
	cc: string;
	bcc: string;
	replyTo: string;
	configurationSet: string;
	mode: "simple" | "template";
	subject: string;
	text: string;
	html: string;
	template: string;
	data: string;
};

export const emptySend: SendForm = {
	from: "",
	to: "",
	cc: "",
	bcc: "",
	replyTo: "",
	configurationSet: "",
	mode: "simple",
	subject: "",
	text: "",
	html: "",
	template: "",
	data: "{}",
};

export function sendInput(f: SendForm) {
	if (!f.from) throw new Error("Choose a verified sender.");
	const to = addressList(f.to),
		cc = addressList(f.cc),
		bcc = addressList(f.bcc),
		replyTo = addressList(f.replyTo);
	for (const [list, label, required] of [
		[to, "recipient", false],
		[cc, "Cc", false],
		[bcc, "Bcc", false],
		[replyTo, "reply-to", false],
	] as const) {
		const problem = validateAddresses(list, label, required);
		if (problem) throw new Error(problem);
	}
	const total = to.length + cc.length + bcc.length;
	if (total === 0) throw new Error("Enter at least one recipient.");
	if (total > maxRecipients)
		throw new Error(`A message has at most ${maxRecipients} recipients.`);
	let Content: Record<string, unknown>;
	if (f.mode === "template") {
		if (!f.template) throw new Error("Choose a template.");
		Content = {
			Template: {
				TemplateName: f.template,
				TemplateData: parseTemplateData(f.data),
			},
		};
	} else {
		if (!f.subject.trim()) throw new Error("Enter a subject.");
		if (!f.text.trim() && !f.html.trim())
			throw new Error("Enter a text body, an HTML body or both.");
		Content = {
			Simple: {
				Subject: { Data: f.subject, Charset: "UTF-8" },
				Body: {
					...(f.text.trim()
						? { Text: { Data: f.text, Charset: "UTF-8" } }
						: {}),
					...(f.html.trim()
						? { Html: { Data: f.html, Charset: "UTF-8" } }
						: {}),
				},
			},
		};
	}
	return {
		FromEmailAddress: f.from,
		Destination: {
			...(to.length ? { ToAddresses: to } : {}),
			...(cc.length ? { CcAddresses: cc } : {}),
			...(bcc.length ? { BccAddresses: bcc } : {}),
		},
		...(replyTo.length ? { ReplyToAddresses: replyTo } : {}),
		...(f.configurationSet ? { ConfigurationSetName: f.configurationSet } : {}),
		Content,
	};
}

export type ConfigSetForm = {
	name: string;
	sendingEnabled: boolean;
	reputationMetrics: boolean;
	tlsPolicy: string;
};

export const emptyConfigSet: ConfigSetForm = {
	name: "",
	sendingEnabled: true,
	reputationMetrics: false,
	tlsPolicy: "OPTIONAL",
};

export function createConfigSetInput(
	f: ConfigSetForm,
	tags: Record<string, string> = {},
) {
	const problem = validateConfigSetName(f.name);
	if (problem) throw new Error(problem);
	return {
		ConfigurationSetName: f.name,
		SendingOptions: { SendingEnabled: f.sendingEnabled },
		ReputationOptions: { ReputationMetricsEnabled: f.reputationMetrics },
		DeliveryOptions: { TlsPolicy: f.tlsPolicy },
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

export type DestinationKind = "sns" | "eventbridge" | "firehose";

export type DestinationForm = {
	name: string;
	kind: DestinationKind;
	enabled: boolean;
	events: string[];
	topicArn: string;
	busArn: string;
	streamArn: string;
	roleArn: string;
};

export const emptyDestination: DestinationForm = {
	name: "",
	kind: "sns",
	enabled: true,
	events: ["SEND"],
	topicArn: "",
	busArn: "",
	streamArn: "",
	roleArn: "",
};

export type EventDestination = {
	Name?: string;
	Enabled?: boolean;
	MatchingEventTypes?: string[];
	SnsDestination?: { TopicArn?: string };
	EventBridgeDestination?: { EventBusArn?: string };
	KinesisFirehoseDestination?: {
		IamRoleArn?: string;
		DeliveryStreamArn?: string;
	};
};

export const destinationKinds: { value: DestinationKind; label: string }[] = [
	{ value: "sns", label: "SNS topic" },
	{ value: "eventbridge", label: "EventBridge event bus" },
	{ value: "firehose", label: "Firehose delivery stream" },
];

export function destinationKind(d: EventDestination): DestinationKind {
	if (d.EventBridgeDestination) return "eventbridge";
	if (d.KinesisFirehoseDestination) return "firehose";
	return "sns";
}

export function destinationTarget(d: EventDestination) {
	return (
		d.SnsDestination?.TopicArn ??
		d.EventBridgeDestination?.EventBusArn ??
		d.KinesisFirehoseDestination?.DeliveryStreamArn ??
		""
	);
}

export function destinationForm(d: EventDestination): DestinationForm {
	return {
		name: d.Name ?? "",
		kind: destinationKind(d),
		enabled: d.Enabled ?? false,
		events: d.MatchingEventTypes ?? [],
		topicArn: d.SnsDestination?.TopicArn ?? "",
		busArn: d.EventBridgeDestination?.EventBusArn ?? "",
		streamArn: d.KinesisFirehoseDestination?.DeliveryStreamArn ?? "",
		roleArn: d.KinesisFirehoseDestination?.IamRoleArn ?? "",
	};
}

function requireArn(value: string, label: string) {
	if (!arnPattern.test(value)) throw new Error(`${label} must be a full ARN.`);
	return value;
}

// destinationDefinition builds the definition, which names exactly one target.
export function destinationDefinition(f: DestinationForm) {
	if (f.events.length === 0) throw new Error("Choose at least one event type.");
	const target =
		f.kind === "sns"
			? {
					SnsDestination: { TopicArn: requireArn(f.topicArn, "The SNS topic") },
				}
			: f.kind === "eventbridge"
				? {
						EventBridgeDestination: {
							EventBusArn: requireArn(f.busArn, "The event bus"),
						},
					}
				: {
						KinesisFirehoseDestination: {
							IamRoleArn: requireArn(f.roleArn, "The IAM role"),
							DeliveryStreamArn: requireArn(f.streamArn, "The delivery stream"),
						},
					};
	return { Enabled: f.enabled, MatchingEventTypes: f.events, ...target };
}

export function createDestinationInput(set: string, f: DestinationForm) {
	if (!/^[A-Za-z0-9_-]{1,64}$/.test(f.name))
		throw new Error(
			"The destination name needs 1 to 64 letters, digits, hyphens or underscores.",
		);
	return {
		ConfigurationSetName: set,
		EventDestinationName: f.name,
		EventDestination: destinationDefinition(f),
	};
}

export function updateDestinationInput(set: string, f: DestinationForm) {
	return {
		ConfigurationSetName: set,
		EventDestinationName: f.name,
		EventDestination: destinationDefinition(f),
	};
}

export type MailRecord = {
	id: string;
	time: string;
	service: string;
	source?: string;
	to: string;
	subject?: string;
	body?: string;
	account?: string;
	region?: string;
};

// sesMessages keeps the messages SES captured for one account and Region, newest first.
export function sesMessages<T extends MailRecord>(
	messages: T[],
	account: string,
	region: string,
): T[] {
	return messages
		.filter(
			(m) =>
				m.service === "ses" && m.account === account && m.region === region,
		)
		.sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
}

export type SendQuota = {
	Max24HourSend?: number;
	MaxSendRate?: number;
	SentLast24Hours?: number;
};

export function quotaText(q?: SendQuota) {
	if (!q) return "-";
	return `${q.SentLast24Hours ?? 0} of ${q.Max24HourSend ?? 0} sent in the last 24 hours, up to ${q.MaxSendRate ?? 0} per second`;
}

export function dkimSummary(d?: {
	SigningEnabled?: boolean;
	Status?: string;
	Tokens?: string[];
}) {
	if (!d) return "-";
	const tokens = d.Tokens?.length ? `, ${d.Tokens.length} tokens` : "";
	return `${d.Status ?? "UNKNOWN"}, signing ${d.SigningEnabled ? "enabled" : "disabled"}${tokens}`;
}

// unixTime shows a timestamp the API gives in seconds.
export function unixTime(seconds?: number) {
	return seconds ? new Date(seconds * 1000).toLocaleString() : "-";
}

// senderOptions lists the addresses a message may be sent from: each email
// identity, and a no-reply address at each domain identity.
export function senderOptions(
	identities: { IdentityName?: string; IdentityType?: string }[],
): { value: string; label: string }[] {
	return identities.flatMap((i) => {
		const name = i.IdentityName ?? "";
		if (!name) return [];
		if (!name.includes("@"))
			return [
				{
					value: `no-reply@${name}`,
					label: `no-reply@${name} (domain ${name})`,
				},
			];
		return [{ value: name, label: name }];
	});
}
