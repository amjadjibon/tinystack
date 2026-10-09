import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
import { type ActionStep, errorText, jsonObject } from "./consoleActions";

export type SfnScope = {
	account: string;
	region: string;
	machine?: string;
	execution?: string;
};
export type Machine = {
	Name: string;
	StateMachineArn: string;
	Type: string;
	Status: string;
	Definition: string;
	RoleArn: string;
	CreationDate: number;
	RevisionId: string;
	LoggingConfiguration?: {
		Level?: string;
		IncludeExecutionData?: boolean;
		Destinations?: { CloudWatchLogsLogGroup?: { LogGroupArn?: string } }[];
	};
	TracingConfiguration?: { Enabled?: boolean };
};
export type Execution = {
	Name: string;
	ExecutionArn: string;
	StateMachineArn: string;
	Status: string;
	StartDate: number;
	StopDate?: number;
	Input?: string;
	Output?: string;
	Error?: string;
	Cause?: string;
	StateMachineVersionArn?: string;
	StateMachineAliasArn?: string;
};
export type HistoryEvent = {
	Id: number;
	PreviousEventId: number;
	Type: string;
	Timestamp: number;
	[key: string]: unknown;
};
export type DefinitionValidation = {
	Result: string;
	Truncated?: boolean;
	Diagnostics?: {
		Severity: string;
		Code: string;
		Message: string;
		Location?: string;
	}[];
};
export const sampleDefinition = JSON.stringify(
	{
		Comment: "Pass execution input through unchanged",
		StartAt: "Hello",
		States: { Hello: { Type: "Pass", End: true } },
	},
	null,
	2,
);
export const machineArn = (s: SfnScope) =>
	`arn:aws:states:${s.region}:${s.account}:stateMachine:${s.machine}`;
export const executionArn = (s: SfnScope) =>
	`arn:aws:states:${s.region}:${s.account}:execution:${s.machine}:${s.execution}`;
export function sfnHref(s: SfnScope, create = false) {
	const p = new URLSearchParams({ account: s.account, region: s.region });
	if (s.machine) p.set("machine", s.machine);
	if (s.execution) p.set("execution", s.execution);
	if (create) p.set("create", "true");
	return `/services/sfn?${p}`;
}
export function sfnCall<T = Record<string, unknown>>(
	s: SfnScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("sfn", operation, s.account, input, s.region);
}
export function machineCall<T = Record<string, unknown>>(
	s: SfnScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return sfnCall(s, operation, { StateMachineArn: machineArn(s), ...input });
}
export function executionCall<T = Record<string, unknown>>(
	s: SfnScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return sfnCall(s, operation, { ExecutionArn: executionArn(s), ...input });
}
export const timestamp = (v?: number) =>
	v === undefined ? "—" : new Date(v * 1000).toISOString();
export const sdkTags = (v: Record<string, string>) =>
	Object.entries(v).map(([Key, Value]) => ({ Key, Value }));
export function tagPlan(
	s: SfnScope,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	const ResourceArn = machineArn(s);
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () =>
							sfnCall(s, "UntagResource", { ResourceArn, TagKeys: removed }),
					},
				]
			: []),
		...(Object.keys(tags).length
			? [
					{
						label: "Save tags",
						run: () =>
							sfnCall(s, "TagResource", { ResourceArn, Tags: sdkTags(tags) }),
					},
				]
			: []),
	];
}
export async function validateDefinition(
	s: SfnScope,
	Definition: string,
	Type: string,
) {
	jsonObject(Definition, "Definition");
	if (new TextEncoder().encode(Definition).length > 1024 * 1024)
		throw new Error("Definition must be at most 1 MiB.");
	return sfnCall<DefinitionValidation>(s, "ValidateStateMachineDefinition", {
		Definition,
		Type,
		MaxResults: 100,
	});
}
export function executionInput(text: string) {
	try {
		JSON.parse(text);
	} catch {
		throw new Error("Execution input must be valid JSON.");
	}
	if (new TextEncoder().encode(text).length > 256 * 1024)
		throw new Error("Execution input must be at most 256 KiB.");
	return text;
}
export function eventDetails(event: HistoryEvent) {
	return Object.fromEntries(
		Object.entries(event).filter(
			([key, value]) => key.endsWith("EventDetails") && value !== null,
		),
	);
}
export function eventState(event: HistoryEvent) {
	for (const key of ["StateEnteredEventDetails", "StateExitedEventDetails"]) {
		const details = event[key] as { Name?: string } | undefined;
		if (details?.Name) return details.Name;
	}
	return "—";
}
// Page reads never mutate resources. Ignore stale responses after page changes or unmount.
export function useSfnPage<T>(
	fetchPage: (token?: string) => Promise<{ rows: T[]; next?: string }>,
	live = false,
) {
	const [tokens, setTokens] = useState<(string | undefined)[]>([undefined]);
	const [page, setPage] = useState<{ rows: T[]; next?: string }>();
	const [loading, setLoading] = useState(true),
		[error, setError] = useState("");
	const token = tokens.at(-1),
		fetcher = useRef(fetchPage),
		sequence = useRef(0),
		mounted = useRef(false);
	fetcher.current = fetchPage;
	const load = useCallback(async () => {
		const id = ++sequence.current;
		setLoading(true);
		try {
			const result = await fetcher.current(token);
			if (mounted.current && id === sequence.current) {
				setPage(result);
				setError("");
			}
		} catch (err) {
			if (mounted.current && id === sequence.current) setError(errorText(err));
		} finally {
			if (mounted.current && id === sequence.current) setLoading(false);
		}
	}, [token]);
	useEffect(() => {
		mounted.current = true;
		setPage(undefined);
		void load();
		const timer =
			live && !token
				? setInterval(() => {
						if (document.visibilityState === "visible") void load();
					}, 5000)
				: undefined;
		return () => {
			mounted.current = false;
			sequence.current++;
			if (timer) clearInterval(timer);
		};
	}, [load, live, token]);
	return {
		rows: page?.rows ?? [],
		loading,
		error,
		reload: load,
		page: tokens.length,
		hasNext: !!page?.next,
		hasPrevious: tokens.length > 1,
		next: () => {
			if (page?.next && !loading) setTokens([...tokens, page.next]);
		},
		previous: () => {
			if (tokens.length > 1 && !loading) setTokens(tokens.slice(0, -1));
		},
	};
}
