import { api } from "./api";
import type {
	AccessPoint,
	FileSystem,
	MountTarget,
	MountTargetRow,
} from "./efsHelpers";

export type EFSScope = { account: string; region: string };
export type EFSTab = "filesystems" | "accesspoints";
export const efsTabs: EFSTab[] = ["filesystems", "accesspoints"];

export function efsHref(s: {
	account: string;
	region: string;
	filesystem?: string;
	tab?: EFSTab;
}) {
	const params = new URLSearchParams({ account: s.account, region: s.region });
	if (s.filesystem) params.set("filesystem", s.filesystem);
	else if (s.tab && s.tab !== "filesystems") params.set("tab", s.tab);
	return `/services/efs?${params}`;
}

export function efsCall<T = Record<string, unknown>>(
	s: EFSScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("efs", operation, s.account, input, s.region);
}

// paged collects every page; page gets the token of the one to fetch, none for the first.
async function paged<T>(
	page: (token?: string) => Promise<{ items?: T[]; next?: string }>,
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await page(token);
		items.push(...(out.items ?? []));
		token = out.next;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}

export const listFileSystems = (s: EFSScope) =>
	paged<FileSystem>(async (marker) => {
		const out = await efsCall<{
			FileSystems?: FileSystem[];
			NextMarker?: string;
		}>(s, "DescribeFileSystems", {
			MaxItems: 100,
			...(marker ? { Marker: marker } : {}),
		});
		return { items: out.FileSystems, next: out.NextMarker };
	});

export async function getFileSystem(s: EFSScope, id: string) {
	const out = await efsCall<{ FileSystems?: FileSystem[] }>(
		s,
		"DescribeFileSystems",
		{ FileSystemId: id },
	);
	const fs = out.FileSystems?.[0];
	if (!fs) throw new Error(`File system ${id} was not found.`);
	return fs;
}

// listMountTargets reads each mount target's security groups, which EFS describes apart from it.
export async function listMountTargets(
	s: EFSScope,
	fileSystem: string,
): Promise<MountTargetRow[]> {
	const targets = await paged<MountTarget>(async (marker) => {
		const out = await efsCall<{
			MountTargets?: MountTarget[];
			NextMarker?: string;
		}>(s, "DescribeMountTargets", {
			FileSystemId: fileSystem,
			MaxItems: 100,
			...(marker ? { Marker: marker } : {}),
		});
		return { items: out.MountTargets, next: out.NextMarker };
	});
	return Promise.all(
		targets.map(async (t) => {
			const out = await efsCall<{ SecurityGroups?: string[] }>(
				s,
				"DescribeMountTargetSecurityGroups",
				{ MountTargetId: t.MountTargetId },
			);
			return { ...t, SecurityGroups: out.SecurityGroups ?? [] };
		}),
	);
}

// listAccessPoints lists the access points of a file system, or of every file system when none is named.
export const listAccessPoints = (s: EFSScope, fileSystem?: string) =>
	paged<AccessPoint>(async (token) => {
		const out = await efsCall<{
			AccessPoints?: AccessPoint[];
			NextToken?: string;
		}>(s, "DescribeAccessPoints", {
			...(fileSystem ? { FileSystemId: fileSystem } : {}),
			MaxResults: 100,
			...(token ? { NextToken: token } : {}),
		});
		return { items: out.AccessPoints, next: out.NextToken };
	});
