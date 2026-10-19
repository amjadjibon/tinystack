export const PUBLIC_URL_STATEMENT_ID = "FunctionURLAllowPublicAccess";
export const PUBLIC_INVOKE_STATEMENT_ID = "FunctionURLInvokeAllowPublicAccess";

export const ALLOWED_CORS_METHODS = [
	"*",
	"GET",
	"POST",
	"PUT",
	"DELETE",
	"HEAD",
	"PATCH",
	"OPTIONS",
];

export type FunctionUrlCors = {
	AllowCredentials?: boolean;
	AllowHeaders?: string[];
	AllowMethods?: string[];
	AllowOrigins?: string[];
	ExposeHeaders?: string[];
	MaxAge?: number;
};

export type FunctionUrlConfig = {
	AuthType?: "NONE" | "AWS_IAM" | string;
	Cors?: FunctionUrlCors;
	CreationTime?: string;
	FunctionArn?: string;
	FunctionUrl?: string;
	InvokeMode?: "BUFFERED" | "RESPONSE_STREAM" | string;
	LastModifiedTime?: string;
};

export type FunctionUrlFormState = {
	qualifierType: "latest" | "alias";
	aliasName: string;
	authType: "NONE" | "AWS_IAM";
	invokeMode: "BUFFERED" | "RESPONSE_STREAM";
	enableCors: boolean;
	allowOrigins: string;
	allowMethods: string[];
	allowHeaders: string;
	allowCredentials: boolean;
	maxAge: string;
	exposeHeaders: string;
};

export function extractQualifier(arn?: string): string {
	if (!arn) return "$LATEST";
	const parts = arn.split(":");
	if (parts.length >= 8 && parts[7]) {
		return parts[7];
	}
	return "$LATEST";
}

export function parseCommaList(input: string): string[] {
	return input
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
}

export function parsePolicyStatements(policyJson?: string | null): Array<{
	Sid?: string;
	Effect?: string;
	Action?: string | string[];
	[key: string]: unknown;
}> {
	if (!policyJson) return [];
	try {
		const doc = JSON.parse(policyJson);
		if (!doc || typeof doc !== "object") return [];
		if (Array.isArray(doc.Statement)) return doc.Statement;
		if (doc.Statement && typeof doc.Statement === "object")
			return [doc.Statement];
		return [];
	} catch {
		return [];
	}
}

export function checkPublicPermissions(policyJson?: string | null): {
	hasUrlAccess: boolean;
	hasInvokeAccess: boolean;
	missingSids: string[];
} {
	const statements = parsePolicyStatements(policyJson);
	const hasUrlAccess = statements.some(
		(s) => s.Sid === PUBLIC_URL_STATEMENT_ID,
	);
	const hasInvokeAccess = statements.some(
		(s) => s.Sid === PUBLIC_INVOKE_STATEMENT_ID,
	);
	const missingSids: string[] = [];
	if (!hasUrlAccess) missingSids.push(PUBLIC_URL_STATEMENT_ID);
	if (!hasInvokeAccess) missingSids.push(PUBLIC_INVOKE_STATEMENT_ID);
	return { hasUrlAccess, hasInvokeAccess, missingSids };
}

export function buildPublicPermissionInputs(
	functionName: string,
	qualifier?: string,
	sidsToAdd?: string[],
): Array<Record<string, unknown>> {
	const q =
		qualifier && qualifier !== "$LATEST" ? { Qualifier: qualifier } : {};
	const sids = sidsToAdd ?? [
		PUBLIC_URL_STATEMENT_ID,
		PUBLIC_INVOKE_STATEMENT_ID,
	];
	const inputs: Array<Record<string, unknown>> = [];

	if (sids.includes(PUBLIC_URL_STATEMENT_ID)) {
		inputs.push({
			FunctionName: functionName,
			StatementId: PUBLIC_URL_STATEMENT_ID,
			Action: "lambda:InvokeFunctionUrl",
			Principal: "*",
			FunctionUrlAuthType: "NONE",
			...q,
		});
	}

	if (sids.includes(PUBLIC_INVOKE_STATEMENT_ID)) {
		inputs.push({
			FunctionName: functionName,
			StatementId: PUBLIC_INVOKE_STATEMENT_ID,
			Action: "lambda:InvokeFunction",
			Principal: "*",
			InvokedViaFunctionUrl: true,
			...q,
		});
	}

	return inputs;
}

export function buildRemovePermissionInputs(
	functionName: string,
	qualifier?: string,
	sidsToRemove?: string[],
): Array<Record<string, unknown>> {
	const q =
		qualifier && qualifier !== "$LATEST" ? { Qualifier: qualifier } : {};
	const sids = sidsToRemove ?? [
		PUBLIC_URL_STATEMENT_ID,
		PUBLIC_INVOKE_STATEMENT_ID,
	];
	return sids.map((sid) => ({
		FunctionName: functionName,
		StatementId: sid,
		...q,
	}));
}

export function corsSummary(cors?: FunctionUrlCors): string {
	if (!cors) return "Disabled";
	const parts: string[] = [];
	const origins =
		cors.AllowOrigins && cors.AllowOrigins.length > 0
			? cors.AllowOrigins.join(", ")
			: "*";
	parts.push(`Origins: ${origins}`);

	const methods =
		cors.AllowMethods && cors.AllowMethods.length > 0
			? cors.AllowMethods.join(", ")
			: "*";
	parts.push(`Methods: ${methods}`);

	if (cors.AllowHeaders && cors.AllowHeaders.length > 0) {
		parts.push(`Headers: ${cors.AllowHeaders.join(", ")}`);
	}
	if (cors.AllowCredentials) {
		parts.push("Credentials: yes");
	}
	if (cors.MaxAge !== undefined && cors.MaxAge !== null) {
		parts.push(`Max age: ${cors.MaxAge}s`);
	}
	return parts.join(" · ");
}

export function defaultFormState(): FunctionUrlFormState {
	return {
		qualifierType: "latest",
		aliasName: "",
		authType: "NONE",
		invokeMode: "BUFFERED",
		enableCors: false,
		allowOrigins: "*",
		allowMethods: ["*"],
		allowHeaders: "*",
		allowCredentials: false,
		maxAge: "0",
		exposeHeaders: "",
	};
}

export function configToFormState(
	config: FunctionUrlConfig,
	qualifier: string,
): FunctionUrlFormState {
	const isAlias = qualifier !== "$LATEST";
	const cors = config.Cors;
	return {
		qualifierType: isAlias ? "alias" : "latest",
		aliasName: isAlias ? qualifier : "",
		authType: config.AuthType === "AWS_IAM" ? "AWS_IAM" : "NONE",
		invokeMode:
			config.InvokeMode === "RESPONSE_STREAM" ? "RESPONSE_STREAM" : "BUFFERED",
		enableCors: !!cors,
		allowOrigins: cors?.AllowOrigins?.join(", ") ?? "*",
		allowMethods: cors?.AllowMethods ?? ["*"],
		allowHeaders: cors?.AllowHeaders?.join(", ") ?? "*",
		allowCredentials: !!cors?.AllowCredentials,
		maxAge: cors?.MaxAge !== undefined ? String(cors.MaxAge) : "0",
		exposeHeaders: cors?.ExposeHeaders?.join(", ") ?? "",
	};
}

export function validateForm(
	form: FunctionUrlFormState,
	isEdit = false,
): string | null {
	if (!isEdit && form.qualifierType === "alias") {
		const alias = form.aliasName.trim();
		if (!alias) {
			return "Enter an alias name.";
		}
		if (alias === "$LATEST") {
			return "Select '$LATEST' target instead of entering it as an alias name.";
		}
		if (!/^[a-zA-Z0-9_-]{1,128}$/.test(alias)) {
			return "Alias name must be 1 to 128 letters, numbers, hyphens, or underscores.";
		}
	}
	if (form.enableCors) {
		if (form.maxAge.trim()) {
			const age = Number(form.maxAge);
			if (
				!/^\d+$/.test(form.maxAge.trim()) ||
				Number.isNaN(age) ||
				age < 0 ||
				age > 86400
			) {
				return "CORS MaxAge must be an integer from 0 to 86400.";
			}
		}
		if (form.allowMethods.length === 0) {
			return "Select at least one allowed HTTP method for CORS.";
		}
	}
	return null;
}

export function buildCorsConfig(
	form: FunctionUrlFormState,
): FunctionUrlCors | undefined {
	if (!form.enableCors) return undefined;
	const origins = parseCommaList(form.allowOrigins);
	const headers = parseCommaList(form.allowHeaders);
	const exposeHeaders = parseCommaList(form.exposeHeaders);
	const age = form.maxAge.trim() ? Number(form.maxAge.trim()) : 0;
	return {
		AllowOrigins: origins.length > 0 ? origins : ["*"],
		AllowMethods: form.allowMethods.length > 0 ? form.allowMethods : ["*"],
		AllowHeaders: headers.length > 0 ? headers : ["*"],
		AllowCredentials: form.allowCredentials,
		MaxAge: age,
		...(exposeHeaders.length > 0 ? { ExposeHeaders: exposeHeaders } : {}),
	};
}

export function formStateToCreatePayload(
	form: FunctionUrlFormState,
	functionName: string,
): Record<string, unknown> {
	const err = validateForm(form, false);
	if (err) throw new Error(err);
	const qualifier =
		form.qualifierType === "alias" ? form.aliasName.trim() : undefined;
	const cors = buildCorsConfig(form);
	return {
		FunctionName: functionName,
		AuthType: form.authType,
		InvokeMode: form.invokeMode,
		...(qualifier ? { Qualifier: qualifier } : {}),
		...(cors ? { Cors: cors } : {}),
	};
}

export function formStateToUpdatePayload(
	form: FunctionUrlFormState,
	functionName: string,
	qualifier: string,
): Record<string, unknown> {
	const err = validateForm(form, true);
	if (err) throw new Error(err);
	const q = qualifier !== "$LATEST" ? { Qualifier: qualifier } : {};
	const cors = buildCorsConfig(form);
	return {
		FunctionName: functionName,
		AuthType: form.authType,
		InvokeMode: form.invokeMode,
		...q,
		...(cors ? { Cors: cors } : {}),
	};
}
