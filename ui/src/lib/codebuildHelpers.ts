import { integer, jsonObject, stringMap } from "./consoleActions.ts";
import { type Tag, tagList, tagMap } from "./tags.ts";
export type Scope = { account: string; region: string };
export type Variable = { Name: string; Value: string; Type: string };
export type Project = {
	Name: string;
	Arn?: string;
	Created?: number;
	LastModified?: number;
	Description?: string;
	Source: {
		Type: string;
		Location?: string;
		Buildspec?: string;
		GitCloneDepth?: number;
	};
	SourceVersion?: string;
	Environment: {
		Type: string;
		Image: string;
		ComputeType: string;
		PrivilegedMode?: boolean;
		EnvironmentVariables?: Variable[];
	};
	Artifacts: Record<string, unknown>;
	ServiceRole: string;
	TimeoutInMinutes?: number;
	QueuedTimeoutInMinutes?: number;
	Tags?: Tag[];
	LogsConfig?: {
		CloudWatchLogs?: {
			Status: string;
			GroupName?: string;
			StreamName?: string;
		};
	};
};
export type Build = {
	Id: string;
	Arn?: string;
	ProjectName: string;
	BuildStatus: string;
	CurrentPhase?: string;
	BuildComplete?: boolean;
	StartTime?: number;
	EndTime?: number;
	SourceVersion?: string;
	Environment?: Project["Environment"];
	Phases?: {
		PhaseType?: string;
		PhaseStatus?: string;
		StartTime?: number;
		EndTime?: number;
		DurationInSeconds?: number;
		Contexts?: { Message?: string }[];
	}[];
	Logs?: { GroupName?: string; StreamName?: string };
};
export const pending = (b: Build) => b.BuildStatus === "IN_PROGRESS";
export function codebuildHref(
	s: Scope,
	project = "",
	build = "",
	tab = "projects",
) {
	const p = new URLSearchParams({ account: s.account, region: s.region, tab });
	if (project) p.set("project", project);
	if (build) p.set("build", build);
	return `/services/codebuild?${p}`;
}
export function variables(text: string): Variable[] {
	const value: unknown = JSON.parse(text);
	if (!Array.isArray(value) || value.length > 100)
		throw new Error(
			"Environment variables must be a JSON array of at most 100 entries.",
		);
	const seen = new Set<string>();
	return value.map((v) => {
		if (
			!v ||
			typeof v !== "object" ||
			typeof v.Name !== "string" ||
			!/^[A-Za-z_][A-Za-z0-9_]{0,255}$/.test(v.Name) ||
			v.Name.startsWith("CODEBUILD_") ||
			seen.has(v.Name) ||
			typeof v.Value !== "string" ||
			v.Value.length > 4096 ||
			!["PLAINTEXT", "PARAMETER_STORE", "SECRETS_MANAGER"].includes(
				v.Type ?? "PLAINTEXT",
			) ||
			Object.keys(v).some((k) => !["Name", "Value", "Type"].includes(k))
		)
			throw new Error(
				"Use unique shell variable names, string values and PLAINTEXT, PARAMETER_STORE or SECRETS_MANAGER types.",
			);
		seen.add(v.Name);
		return { Name: v.Name, Value: v.Value, Type: v.Type ?? "PLAINTEXT" };
	});
}
export function projectForm(p?: Project) {
	return {
		name: p?.Name ?? "",
		description: p?.Description ?? "",
		source: p?.Source.Type ?? "NO_SOURCE",
		location: p?.Source.Location ?? "",
		buildspec:
			p?.Source.Buildspec ??
			"version: 0.2\nphases:\n  build:\n    commands:\n      - echo Hello\n",
		sourceVersion: p?.SourceVersion ?? "",
		image: p?.Environment.Image ?? "alpine:3.21",
		compute: p?.Environment.ComputeType ?? "BUILD_GENERAL1_SMALL",
		environment: p?.Environment.Type ?? "LINUX_CONTAINER",
		privileged: p?.Environment.PrivilegedMode ?? false,
		role: p?.ServiceRole ?? "",
		timeout: String(p?.TimeoutInMinutes ?? 60),
		queued: String(p?.QueuedTimeoutInMinutes ?? 60),
		vars: JSON.stringify(p?.Environment.EnvironmentVariables ?? [], null, 2),
		tags: JSON.stringify(tagMap(p?.Tags), null, 2),
		artifacts: JSON.stringify(
			p?.Artifacts ?? { Type: "NO_ARTIFACTS" },
			null,
			2,
		),
		logs: JSON.stringify(
			p?.LogsConfig ?? { CloudWatchLogs: { Status: "ENABLED" } },
			null,
			2,
		),
	};
}
export function projectInput(f: ReturnType<typeof projectForm>, p?: Project) {
	if (!/^[A-Za-z0-9][A-Za-z0-9_-]{1,149}$/.test(f.name))
		throw new Error(
			"Name requires 2–150 letters, digits, hyphens or underscores, starting with a letter or digit.",
		);
	return {
		Name: f.name,
		Description: f.description,
		Source: {
			...p?.Source,
			Type: f.source,
			Location: ["NO_SOURCE", "CODEPIPELINE"].includes(f.source)
				? undefined
				: f.location,
			Buildspec: f.buildspec,
			...(["NO_SOURCE", "CODEPIPELINE", "S3"].includes(f.source)
				? { GitCloneDepth: undefined }
				: {}),
		},
		SourceVersion: f.sourceVersion,
		Environment: {
			Type: f.environment,
			Image: f.image,
			ComputeType: f.compute,
			PrivilegedMode: f.privileged,
			EnvironmentVariables: variables(f.vars),
		},
		ServiceRole: f.role,
		Artifacts: jsonObject(f.artifacts, "Artifacts"),
		TimeoutInMinutes: integer(f.timeout, "Timeout", 5, 2160),
		QueuedTimeoutInMinutes: integer(f.queued, "Queued timeout", 5, 480),
		Tags: tagList(stringMap(f.tags, "Tags")),
		LogsConfig: jsonObject(f.logs, "Logs configuration"),
	};
}

export type ReportGroup = {
	Arn: string;
	Name: string;
	Type: string;
	Created?: number;
	Tags?: Tag[];
};
export type Report = {
	Arn: string;
	Name: string;
	Status: string;
	Created?: number;
	TestSummary?: { Total: number; StatusCounts: Record<string, number> };
};
export type TestCase = {
	Name: string;
	Status: string;
	TestSuiteName?: string;
	DurationInNanoSeconds?: number;
	Message?: string;
};
export function reportGroupInput(name: string, tags: string, arn?: string) {
	if (!arn && !/^[A-Za-z0-9][A-Za-z0-9_-]{1,127}$/.test(name))
		throw new Error(
			"Report group name requires 2–128 letters, digits, hyphens or underscores.",
		);
	return {
		...(arn ? { Arn: arn } : { Name: name, Type: "TEST" }),
		ExportConfig: { ExportConfigType: "NO_EXPORT" },
		Tags: tagList(stringMap(tags, "Tags")),
	};
}
