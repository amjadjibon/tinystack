import { api } from "./api";

export type KMSScope = { account: string; region: string; key?: string };
export type KMSKey = {
	KeyId: string;
	Arn: string;
	Description?: string;
	KeyState?: string;
	KeySpec?: string;
	KeyUsage?: string;
	KeyManager?: string;
	CreationDate?: number;
	EncryptionAlgorithms?: string[];
	MultiRegion?: boolean;
	DeletionDate?: number;
};
export type KMSAlias = { AliasName: string; TargetKeyId?: string };
export type KMSTag = { TagKey: string; TagValue: string };
export function kmsHref(s: KMSScope) {
	const params = new URLSearchParams({ account: s.account, region: s.region });
	if (s.key) params.set("key", s.key);
	return `/services/kms?${params}`;
}
export function kmsCall<T = Record<string, unknown>>(
	s: KMSScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("kms", operation, s.account, input, s.region);
}
export async function kmsPages<T>(
	s: KMSScope,
	operation: string,
	field: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await kmsCall<Record<string, unknown>>(s, operation, {
			...input,
			// 50 is the largest page every KMS list accepts (ListResourceTags caps at 50).
			Limit: 50,
			...(marker ? { Marker: marker } : {}),
		});
		items.push(...((out[field] as T[] | undefined) ?? []));
		if (!out.Truncated) break;
		marker = out.NextMarker as string | undefined;
		if (!marker || seen.has(marker))
			throw new Error("The server returned an invalid page marker.");
		seen.add(marker);
	} while (marker);
	return items;
}
export const kmsSpecs = [
	"SYMMETRIC_DEFAULT",
	"HMAC_224",
	"HMAC_256",
	"HMAC_384",
	"HMAC_512",
	"RSA_2048",
	"RSA_3072",
	"RSA_4096",
	"ECC_NIST_P256",
	"ECC_NIST_P384",
	"ECC_NIST_P521",
	"ECC_NIST_EDWARDS25519",
];
export function kmsUsages(spec: string) {
	return spec.startsWith("HMAC_")
		? ["GENERATE_VERIFY_MAC"]
		: spec.startsWith("RSA_")
			? ["ENCRYPT_DECRYPT", "SIGN_VERIFY"]
			: spec.startsWith("ECC_")
				? ["SIGN_VERIFY"]
				: ["ENCRYPT_DECRYPT"];
}
export function kmsEncode(value: string) {
	return btoa(
		Array.from(new TextEncoder().encode(value), (b) =>
			String.fromCharCode(b),
		).join(""),
	);
}
export function kmsDecode(value: string) {
	return new TextDecoder("utf-8", { fatal: true }).decode(
		Uint8Array.from(atob(value), (c) => c.charCodeAt(0)),
	);
}
