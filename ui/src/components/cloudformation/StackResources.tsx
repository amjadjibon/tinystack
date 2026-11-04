import {
	Alert,
	Box,
	Button,
	Header,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { resourceHref } from "#/components/ResourceTable";
import type { Resource } from "#/lib/api";
import { type CfnScope, cfnCall, cfnHref } from "#/lib/cloudformation";
import {
	cfnDate,
	findInventory,
	nestedStackType,
	type StackResource,
} from "#/lib/cloudformationHelpers";
import { useFollow } from "#/lib/useFollow";
import type { Poll } from "#/lib/usePoll";
import { StackStatus } from "./StackStatus";

// StackResources lists the stack's resources. A physical ID links to the
// dedicated service page, or the generic resource page, when the inventory knows it.
export function StackResources({
	scope,
	poll,
	inventory,
}: {
	scope: CfnScope;
	poll: Poll<StackResource[]>;
	inventory: Resource[];
}) {
	const follow = useFollow();
	const [detail, setDetail] = useState("");
	const [error, setError] = useState("");
	const resources = poll.data ?? [];
	return (
		<SpaceBetween size="s">
			{!!error && <Alert type="error">{error}</Alert>}
			<Button
				onClick={async () => {
					try {
						setDetail(
							JSON.stringify(
								await cfnCall(scope, "DescribeStackResources", {
									StackName: scope.stack,
								}),
								null,
								2,
							),
						);
						setError("");
					} catch (err) {
						setError(String(err));
					}
				}}
			>
				Describe all resources
			</Button>
			{!!detail && <CodeBlock text={detail} />}
			{!!poll.error && (
				<Alert type="error" header="Cannot load resources">
					{poll.error}
				</Alert>
			)}
			<Table
				items={resources}
				trackBy="LogicalResourceId"
				loading={poll.loading}
				loadingText="Loading resources"
				variant="embedded"
				header={
					<Header
						counter={`(${resources.length})`}
						actions={
							<Button
								iconName="refresh"
								ariaLabel="Refresh resources"
								onClick={poll.reload}
							/>
						}
					>
						Resources
					</Header>
				}
				empty={
					<Box textAlign="center" padding="l">
						This stack has no resources.
					</Box>
				}
				columnDefinitions={[
					{
						id: "describe",
						header: "Details",
						cell: (r) => (
							<Button
								onClick={async () => {
									try {
										setDetail(
											JSON.stringify(
												await cfnCall(scope, "DescribeStackResource", {
													StackName: scope.stack,
													LogicalResourceId: r.LogicalResourceId,
												}),
												null,
												2,
											),
										);
										setError("");
									} catch (err) {
										setError(String(err));
									}
								}}
							>
								Describe
							</Button>
						),
					},
					{
						id: "logical",
						header: "Logical ID",
						isRowHeader: true,
						cell: (r) => r.LogicalResourceId,
					},
					{
						id: "physical",
						header: "Physical ID",
						cell: (r) => {
							const found = findInventory(
								inventory,
								r,
								scope.account,
								scope.region,
							);
							if (found)
								return (
									<Link href={resourceHref(found)} onFollow={follow}>
										{r.PhysicalResourceId}
									</Link>
								);
							return r.PhysicalResourceId || "—";
						},
					},
					{ id: "type", header: "Type", cell: (r) => r.ResourceType },
					{
						id: "status",
						header: "Status",
						cell: (r) => <StackStatus status={r.ResourceStatus} />,
					},
					{
						id: "reason",
						header: "Status reason",
						cell: (r) => r.ResourceStatusReason ?? "—",
					},
					{
						id: "updated",
						header: "Last updated",
						cell: (r) => cfnDate(r.LastUpdatedTimestamp),
					},
				]}
			/>
		</SpaceBetween>
	);
}

// NestedStacks links the AWS::CloudFormation::Stack resources to their stack pages.
export function NestedStacks({
	scope,
	poll,
}: {
	scope: CfnScope;
	poll: Poll<StackResource[]>;
}) {
	const follow = useFollow();
	const nested = (poll.data ?? []).filter(
		(r) => r.ResourceType === nestedStackType,
	);
	return (
		<SpaceBetween size="s">
			{!!poll.error && (
				<Alert type="error" header="Cannot load nested stacks">
					{poll.error}
				</Alert>
			)}
			<Table
				items={nested}
				trackBy="LogicalResourceId"
				loading={poll.loading}
				loadingText="Loading nested stacks"
				variant="embedded"
				header={<Header counter={`(${nested.length})`}>Nested stacks</Header>}
				empty={
					<Box textAlign="center" padding="l">
						This stack has no nested stacks.
					</Box>
				}
				columnDefinitions={[
					{
						id: "logical",
						header: "Logical ID",
						isRowHeader: true,
						cell: (r) => r.LogicalResourceId,
					},
					{
						id: "stack",
						header: "Stack",
						cell: (r) =>
							r.PhysicalResourceId ? (
								<Link
									href={cfnHref({ ...scope, stack: r.PhysicalResourceId })}
									onFollow={follow}
								>
									{r.PhysicalResourceId}
								</Link>
							) : (
								"—"
							),
					},
					{
						id: "status",
						header: "Status",
						cell: (r) => <StackStatus status={r.ResourceStatus} />,
					},
				]}
			/>
		</SpaceBetween>
	);
}
