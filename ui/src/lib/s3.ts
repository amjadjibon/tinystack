import { api } from "./api";

export type S3Scope = { account: string; region: string; bucket: string };
export type S3Object = {
	Key: string;
	Size?: number;
	LastModified?: string;
	ETag?: string;
	VersionId?: string;
	IsLatest?: boolean;
	folder?: boolean;
	marker?: boolean;
};
export type S3Listing = {
	Contents?: S3Object[];
	CommonPrefixes?: { Prefix: string }[];
	Versions?: S3Object[];
	DeleteMarkers?: S3Object[];
	IsTruncated?: boolean;
	NextContinuationToken?: string;
	NextKeyMarker?: string;
	NextVersionIdMarker?: string;
};
export const uploadLimit = 1024 * 1024;
export const validRegion = (value: string) =>
	/^[a-z]{2}(?:-[a-z]+)+-\d+$/.test(value);
export function s3Href(
	scope: Partial<S3Scope> = {},
	prefix = "",
	key = "",
	version = "",
) {
	return `/services/s3?${new URLSearchParams({ ...scope, prefix, key, version })}`;
}
export function s3Call<T = Record<string, unknown>>(
	scope: S3Scope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action<T>(
		"s3",
		operation,
		scope.account,
		{ ...(scope.bucket ? { Bucket: scope.bucket } : {}), ...input },
		scope.region,
	);
}
export function sizeLabel(bytes = 0) {
	if (bytes < 1024) return `${bytes} B`;
	if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
	return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
export function objectID(object: S3Object) {
	return JSON.stringify([
		object.Key,
		object.VersionId ?? "",
		!!object.folder,
		!!object.marker,
	]);
}
export async function fileBody(file: File): Promise<string> {
	if (file.size > uploadLimit)
		throw new Error(`${file.name} exceeds the 1 MiB console upload limit.`);
	const bytes = new Uint8Array(await file.arrayBuffer());
	let text = "";
	for (let i = 0; i < bytes.length; i += 8192)
		text += String.fromCharCode(...bytes.subarray(i, i + 8192));
	return btoa(text);
}
export async function downloadObject(scope: S3Scope, object: S3Object) {
	const result = await s3Call<{ Body: string; ContentType?: string }>(
		scope,
		"GetObject",
		{
			Key: object.Key,
			...(object.VersionId ? { VersionId: object.VersionId } : {}),
		},
	);
	const bytes = Uint8Array.from(atob(result.Body ?? ""), (character) =>
		character.charCodeAt(0),
	);
	// Always download through a blob; object HTML never renders in the console's origin.
	const url = URL.createObjectURL(
		new Blob([bytes], {
			type: result.ContentType || "application/octet-stream",
		}),
	);
	const link = document.createElement("a");
	link.href = url;
	link.download = object.Key.split("/").pop() || "object";
	document.body.append(link);
	link.click();
	link.remove();
	setTimeout(() => URL.revokeObjectURL(url), 60000);
}
export type S3Step = { label: string; run: () => Promise<unknown> };
export async function s3Sequence(steps: S3Step[], completed: Set<number>) {
	for (const [index, step] of steps.entries()) {
		if (completed.has(index)) continue;
		try {
			await step.run();
			completed.add(index);
		} catch (error) {
			throw new Error(
				`${completed.size} of ${steps.length} steps completed. ${step.label}: ${error instanceof Error ? error.message : error}. Retry continues remaining steps while this dialog stays open.`,
			);
		}
	}
}
export async function deleteObjects(scope: S3Scope, objects: S3Object[]) {
	const out = await s3Call<{
		Errors?: { Key: string; Code: string; Message: string }[];
	}>(scope, "DeleteObjects", {
		Delete: {
			Objects: objects.map(({ Key, VersionId }) => ({
				Key,
				...(VersionId ? { VersionId } : {}),
			})),
		},
	});
	if (out.Errors?.length)
		throw new Error(
			out.Errors.map((e) => `${e.Key}: ${e.Code} ${e.Message}`).join("; "),
		);
}

export async function currentObjects(scope: S3Scope, prefix: string) {
	const objects: S3Object[] = [];
	let token: string | undefined;
	for (;;) {
		const page = await s3Call<S3Listing>(scope, "ListObjectsV2", {
			Prefix: prefix,
			MaxKeys: 1000,
			...(token ? { ContinuationToken: token } : {}),
		});
		objects.push(...(page.Contents ?? []));
		if (!page.IsTruncated) return objects;
		if (!page.NextContinuationToken)
			throw new Error("The object listing returned no continuation token.");
		token = page.NextContinuationToken;
	}
}
export async function emptyBucketSteps(scope: S3Scope): Promise<S3Step[]> {
	// Read every page before deleting: mutating while paging invalidates positional markers.
	const objects: S3Object[] = [];
	let markers: Record<string, unknown> = {};
	for (;;) {
		const page = await s3Call<S3Listing>(scope, "ListObjectVersions", {
			...markers,
			MaxKeys: 1000,
		});
		objects.push(...(page.Versions ?? []), ...(page.DeleteMarkers ?? []));
		if (!page.IsTruncated) break;
		if (!page.NextKeyMarker)
			throw new Error("The version listing returned no continuation marker.");
		markers = {
			KeyMarker: page.NextKeyMarker,
			VersionIdMarker: page.NextVersionIdMarker,
		};
	}
	const uploads: { Key: string; UploadId: string }[] = [];
	markers = {};
	for (;;) {
		const page = await s3Call<{
			Uploads?: { Key: string; UploadId: string }[];
			IsTruncated?: boolean;
			NextKeyMarker?: string;
			NextUploadIdMarker?: string;
		}>(scope, "ListMultipartUploads", { ...markers, MaxUploads: 1000 });
		uploads.push(...(page.Uploads ?? []));
		if (!page.IsTruncated) break;
		if (!page.NextKeyMarker)
			throw new Error("The upload listing returned no continuation marker.");
		markers = {
			KeyMarker: page.NextKeyMarker,
			UploadIdMarker: page.NextUploadIdMarker,
		};
	}
	const steps: S3Step[] = [];
	for (let offset = 0; offset < objects.length; offset += 1000) {
		const batch = objects.slice(offset, offset + 1000);
		steps.push({
			label: `Delete versions ${offset + 1}–${offset + batch.length}`,
			run: () => deleteObjects(scope, batch),
		});
	}
	for (const upload of uploads)
		steps.push({
			label: `Abort upload ${upload.Key}`,
			run: () =>
				s3Call(scope, "AbortMultipartUpload", {
					Key: upload.Key,
					UploadId: upload.UploadId,
				}),
		});
	steps.push({
		label: "Check bucket is empty",
		run: async () => {
			const versions = await s3Call<S3Listing>(scope, "ListObjectVersions", {
				MaxKeys: 1,
			});
			const multipart = await s3Call<{ Uploads?: unknown[] }>(
				scope,
				"ListMultipartUploads",
				{ MaxUploads: 1 },
			);
			if (
				versions.Versions?.length ||
				versions.DeleteMarkers?.length ||
				multipart.Uploads?.length
			) {
				throw new Error(
					"The bucket received more data. Close this dialog and empty it again.",
				);
			}
		},
	});
	return steps;
}
