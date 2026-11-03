import { StatusIndicator, Tabs } from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import { batchCall } from "#/lib/batch";
import { type BatchScope, statusType } from "#/lib/batchHelpers";

export type SectionProps = {
	scope: BatchScope;
	enabled: boolean;
	// Whether the Docker runtime runs jobs, so that they change on their own.
	docker: boolean;
	// The name, name:revision or ID of the resource whose page is shown, or "".
	selected: string;
	tab: string;
	onChanged: () => void;
};

// Status shows a state or status with its indicator.
export function Status({ value }: { value?: string }) {
	return (
		<StatusIndicator type={statusType(value)}>{value ?? "-"}</StatusIndicator>
	);
}

// DetailTabs shows a resource as an overview and its tags; the address keeps the tab.
export function DetailTabs({
	tab,
	href,
	overview,
	tags,
}: {
	tab: string;
	href: (tab: string) => string;
	overview: ReactNode;
	tags: ReactNode;
}) {
	const navigate = useNavigate();
	return (
		<Tabs
			activeTabId={tab}
			onChange={({ detail }) => navigate({ href: href(detail.activeTabId) })}
			tabs={[
				{
					id: "overview",
					label: "Overview",
					content: tab === "overview" ? overview : null,
				},
				{ id: "tags", label: "Tags", content: tab === "tags" ? tags : null },
			]}
		/>
	);
}

// ResourceTags edits the tags of an environment, queue, definition revision or
// job, which Batch keeps under the resource's ARN.
export function ResourceTags({
	scope,
	arn,
	tags,
	enabled,
	onChanged,
}: {
	scope: BatchScope;
	arn: string;
	tags?: Record<string, string>;
	enabled: boolean;
	onChanged: () => void;
}) {
	return (
		<ConsoleTags
			tags={tags ?? {}}
			enabled={enabled}
			onChanged={onChanged}
			plan={(next, removed) => [
				...(Object.keys(next).length
					? [
							{
								label: "TagResource",
								run: () =>
									batchCall(scope, "TagResource", {
										ResourceArn: arn,
										Tags: next,
									}),
							},
						]
					: []),
				...(removed.length
					? [
							{
								label: "UntagResource",
								run: () =>
									batchCall(scope, "UntagResource", {
										ResourceArn: arn,
										TagKeys: removed,
									}),
							},
						]
					: []),
			]}
		/>
	);
}
