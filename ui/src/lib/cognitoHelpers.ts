export type Attribute = { Name: string; Value: string };
export type CognitoUser = {
	Username: string;
	Attributes?: Attribute[];
	UserStatus?: string;
	Enabled?: boolean;
};
export function matchesUser(user: CognitoUser, query: string) {
	const needle = query.trim().toLowerCase();
	return [
		user.Username,
		user.UserStatus ?? "",
		...(user.Attributes ?? [])
			.filter((a) => a.Name === "email")
			.map((a) => a.Value),
	].some((v) => v.toLowerCase().includes(needle));
}
export function changedAttributes(
	before: Attribute[],
	values: Record<string, string>,
): Attribute[] {
	return Object.entries(values)
		.filter(
			([name, value]) => before.find((a) => a.Name === name)?.Value !== value,
		)
		.map(([Name, Value]) => ({ Name, Value }));
}

export const poolUpdateKeys = [
	"UserPoolId",
	"Policies",
	"DeletionProtection",
	"AutoVerifiedAttributes",
	"SmsVerificationMessage",
	"EmailVerificationMessage",
	"EmailVerificationSubject",
	"VerificationMessageTemplate",
	"SmsAuthenticationMessage",
	"UserAttributeUpdateSettings",
	"MfaConfiguration",
	"DeviceConfiguration",
	"EmailConfiguration",
	"SmsConfiguration",
	"UserPoolTags",
	"AdminCreateUserConfig",
	"UserPoolAddOns",
	"AccountRecoverySetting",
	"UserPoolTier",
	"PoolName",
	"LambdaConfig",
] as const;

export const clientUpdateKeys = [
	"UserPoolId",
	"ClientId",
	"ClientName",
	"RefreshTokenValidity",
	"AccessTokenValidity",
	"IdTokenValidity",
	"TokenValidityUnits",
	"ReadAttributes",
	"WriteAttributes",
	"ExplicitAuthFlows",
	"SupportedIdentityProviders",
	"CallbackURLs",
	"LogoutURLs",
	"DefaultRedirectURI",
	"AllowedOAuthFlows",
	"AllowedOAuthScopes",
	"AllowedOAuthFlowsUserPoolClient",
	"PreventUserExistenceErrors",
	"EnableTokenRevocation",
	"EnablePropagateAdditionalUserContextData",
	"AuthSessionValidity",
	"RefreshTokenRotation",
] as const;

export function editableSettings(
	value: Record<string, unknown>,
	keys: readonly string[],
) {
	return Object.fromEntries(
		keys.filter((k) => value[k] !== undefined).map((k) => [k, value[k]]),
	);
}
export function removedAttributes(
	before: Attribute[],
	values: Record<string, string>,
) {
	if ("sub" in values) throw new Error("sub is read-only.");
	return before
		.filter((a) => a.Name !== "sub" && !Object.hasOwn(values, a.Name))
		.map((a) => a.Name);
}
export type CognitoField = {
	key: string;
	label: string;
	kind?: "number" | "boolean" | "list" | "json";
	options?: string[];
	required?: boolean;
	min?: number;
	max?: number;
};
export function fieldValue(
	value: Record<string, unknown>,
	path: string,
): unknown {
	return path
		.split(".")
		.reduce<unknown>(
			(v, k) =>
				v && typeof v === "object"
					? (v as Record<string, unknown>)[k]
					: undefined,
			value,
		);
}
export function formInput(
	base: Record<string, unknown>,
	fields: CognitoField[],
	values: Record<string, string>,
) {
	const out = structuredClone(base);
	for (const f of fields) {
		const text = (values[f.key] ?? "").trim();
		if (f.required && !text) throw new Error(`Enter ${f.label}.`);
		let value: unknown = text;
		if (f.kind === "number") {
			if (!text) continue;
			const n = Number(text);
			if (
				!/^\d+$/.test(text) ||
				!Number.isSafeInteger(n) ||
				n < (f.min ?? 0) ||
				n > (f.max ?? 2147483647)
			)
				throw new Error(
					`${f.label} must be an integer from ${f.min ?? 0} to ${f.max ?? 2147483647}.`,
				);
			value = n;
		} else if (f.kind === "boolean") value = text === "true";
		else if (f.kind === "list") {
			value = [
				...new Set(
					text
						.split(/[\n,]/)
						.map((v) => v.trim())
						.filter(Boolean),
				),
			];
			const bad =
				f.options && (value as string[]).find((v) => !f.options?.includes(v));
			if (bad) throw new Error(`${f.label} has an unsupported value: ${bad}.`);
		} else if (f.kind === "json") {
			value = text ? JSON.parse(text) : undefined;
			if (text && (!value || typeof value !== "object" || Array.isArray(value)))
				throw new Error(`${f.label} must be an object.`);
		}
		if (f.kind !== "list" && f.options && text && !f.options.includes(text))
			throw new Error(`Choose ${f.label}.`);
		if (f.kind !== "list" && f.options && !text) value = undefined;
		const parts = f.key.split(".");
		let target = out;
		for (const k of parts.slice(0, -1)) {
			target[k] ??= {};
			target = target[k] as Record<string, unknown>;
		}
		if (value === undefined) delete target[parts[parts.length - 1]];
		else target[parts[parts.length - 1]] = value;
	}
	return out;
}
