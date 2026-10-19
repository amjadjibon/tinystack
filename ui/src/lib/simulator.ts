export type SimulationDecision = "allowed" | "explicitDeny" | "implicitDeny";

export type SimulationStatement = {
	SourcePolicyId?: string;
	SourcePolicyType?: string;
};

export type SimulationBoundaryDetail = {
	AllowedByPermissionsBoundary?: boolean;
};

export type SimulationEvaluationResult = {
	EvalActionName?: string;
	EvalResourceName?: string;
	EvalDecision?: SimulationDecision | string;
	MatchedStatements?: SimulationStatement[];
	MissingContextValues?: string[];
	PermissionsBoundaryDecisionDetail?: SimulationBoundaryDetail;
};

export const CONTEXT_KEY_TYPES = [
	"string",
	"stringList",
	"numeric",
	"numericList",
	"boolean",
	"booleanList",
	"date",
	"dateList",
	"ip",
	"ipList",
	"binary",
	"binaryList",
] as const;

export type ContextKeyType = (typeof CONTEXT_KEY_TYPES)[number];

export type SimulationContextEntry = {
	ContextKeyName: string;
	ContextKeyType: ContextKeyType;
	ContextKeyValues: string[];
};

export type ContextEntryRow = {
	id: string;
	key: string;
	type: ContextKeyType;
	value: string;
};

export function isIamPrincipalArn(arn: string): boolean {
	return /^arn:aws:iam::\d{12}:(user|role|group)\/.+$/.test(arn.trim());
}

export function isIamUserOrRoleArn(arn: string): boolean {
	return /^arn:aws:iam::\d{12}:(user|role)\/.+$/.test(arn.trim());
}

export function parseActions(input: string): {
	actions: string[];
	error?: string;
} {
	const raw = input
		.split(/[\n,]+/)
		.map((s) => s.trim())
		.filter(Boolean);

	if (raw.length === 0) {
		return { actions: [], error: "At least one action is required." };
	}

	for (const action of raw) {
		const parts = action.split(":");
		if (
			parts.length !== 2 ||
			!parts[0] ||
			!parts[1] ||
			action.includes("*") ||
			action.includes("?")
		) {
			return {
				actions: [],
				error: `Action "${action}" is invalid. Actions must use service:action format without wildcards (e.g. s3:GetObject).`,
			};
		}
	}

	return { actions: raw };
}

export function parseResources(input: string): string[] {
	const list = input
		.split(/[\n,]+/)
		.map((s) => s.trim())
		.filter(Boolean);
	return list.length > 0 ? list : ["*"];
}

export function validatePolicyJson(
	jsonString: string,
	label = "Policy document",
): string | null {
	const trimmed = jsonString.trim();
	if (!trimmed) return null;
	try {
		const parsed = JSON.parse(trimmed);
		if (
			typeof parsed !== "object" ||
			parsed === null ||
			Array.isArray(parsed)
		) {
			return `${label} must be a JSON object.`;
		}
		return null;
	} catch (e) {
		return `${label} contains invalid JSON: ${e instanceof Error ? e.message : String(e)}`;
	}
}

export function formatPolicyJson(raw: string): string {
	const trimmed = raw.trim();
	if (!trimmed) return "";
	try {
		return JSON.stringify(JSON.parse(trimmed), null, 2);
	} catch {
		return raw;
	}
}

export function buildContextEntries(rows: ContextEntryRow[]): {
	entries: SimulationContextEntry[];
	error?: string;
} {
	const entries: SimulationContextEntry[] = [];
	for (let i = 0; i < rows.length; i++) {
		const row = rows[i];
		const key = row.key.trim();
		const val = row.value.trim();

		// If both key and value are empty, ignore row
		if (!key && !val) continue;

		if (!key) {
			return { entries: [], error: `Context entry #${i + 1} has no key name.` };
		}
		if (!val) {
			return {
				entries: [],
				error: `Context entry "${key}" requires a value.`,
			};
		}

		const isList = row.type.endsWith("List");
		let values: string[];
		if (isList) {
			values = val
				.split(",")
				.map((s) => s.trim())
				.filter(Boolean);
			if (values.length === 0) {
				return {
					entries: [],
					error: `Context entry "${key}" requires at least one value.`,
				};
			}
		} else {
			values = [val];
		}

		entries.push({
			ContextKeyName: key,
			ContextKeyType: row.type,
			ContextKeyValues: values,
		});
	}
	return { entries };
}

export const defaultPolicyDocument = JSON.stringify(
	{
		Version: "2012-10-17",
		Statement: [
			{
				Effect: "Allow",
				Action: "*",
				Resource: "*",
			},
		],
	},
	null,
	2,
);
