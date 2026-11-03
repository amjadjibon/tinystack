import { Link } from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { Rows } from "#/components/containers/Controls";
import { countText, type JobSummary } from "#/lib/batchHelpers";
import { unixMillis } from "#/lib/format";
import { useFollow } from "#/lib/useFollow";
import { Status } from "./Shared";

// JobRows lists jobs, each linking to its page.
export function JobRows({
	title,
	items,
	loading,
	emptyText,
	actions,
	href,
}: {
	title: string;
	items: JobSummary[];
	loading: boolean;
	emptyText: string;
	actions: ReactNode;
	href: (job: JobSummary) => string;
}) {
	const follow = useFollow();
	return (
		<Rows
			title={title}
			items={items}
			loading={loading}
			emptyText={emptyText}
			actions={actions}
			columns={[
				{
					id: "name",
					header: "Name",
					cell: (j) => (
						<Link href={href(j)} onFollow={follow}>
							{/* An array child is told apart by its index. */}
							{j.ArrayProperties?.Index === undefined
								? (j.JobName ?? j.JobId)
								: `${j.JobName ?? j.JobId}:${j.ArrayProperties.Index}`}
						</Link>
					),
				},
				{ id: "id", header: "Job ID", cell: (j) => j.JobId },
				{
					id: "status",
					header: "Status",
					cell: (j) => <Status value={j.Status} />,
				},
				{
					id: "reason",
					header: "Status reason",
					cell: (j) => j.StatusReason ?? "-",
				},
				{
					id: "created",
					header: "Created",
					cell: (j) => unixMillis(j.CreatedAt),
				},
				{
					id: "started",
					header: "Started",
					cell: (j) => unixMillis(j.StartedAt),
				},
				{
					id: "stopped",
					header: "Stopped",
					cell: (j) => unixMillis(j.StoppedAt),
				},
				{
					id: "exit",
					header: "Exit code",
					cell: (j) => countText(j.Container?.ExitCode),
				},
			]}
		/>
	);
}
