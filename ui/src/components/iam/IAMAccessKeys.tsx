import {
	Alert,
	Button,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { EmptyState } from "#/components/EmptyState";
import { iamInput, iamPages } from "#/lib/iam";
import { usePoll } from "#/lib/usePoll";
import type { IAMActions, IAMSecret } from "./IAMForm";

type KeyMetadata = { AccessKeyId: string; Status: string; CreateDate: string };
export function IAMAccessKeys({
	resource,
	enabled,
	run,
	open,
	onSecret,
}: IAMActions & { onSecret: (key: IAMSecret) => void }) {
	const keys = usePoll(() =>
		enabled
			? iamPages<KeyMetadata>(
					resource.account,
					"ListAccessKeys",
					iamInput(resource),
					"AccessKeyMetadata",
				)
			: Promise.resolve(
					((resource.details?.AccessKeyIds as string[] | undefined) ?? []).map(
						(AccessKeyId) => ({ AccessKeyId, Status: "—", CreateDate: "" }),
					),
				),
	);
	const change = async (operation: string, input = {}) => {
		try {
			return await run(operation, input);
		} finally {
			keys.reload();
		}
	};
	return (
		<SpaceBetween size="l">
			{!!keys.error && <Alert type="error">{keys.error}</Alert>}
			<Table
				items={keys.data ?? []}
				trackBy="AccessKeyId"
				loading={keys.loading}
				header={
					<Header
						variant="h2"
						counter={`(${keys.data?.length ?? 0})`}
						actions={
							<Button
								disabled={
									!enabled ||
									keys.loading ||
									!!keys.error ||
									(keys.data?.length ?? 0) >= 2
								}
								onClick={() =>
									open({
										title: "Create access key",
										confirm: "Create access key",
										description: `Create programmatic credentials for ${resource.name}. The secret is shown once.`,
										submit: async () => {
											const result = await change("CreateAccessKey");
											onSecret(result.AccessKey as IAMSecret);
										},
									})
								}
							>
								Create access key
							</Button>
						}
					>
						Access keys
					</Header>
				}
				columnDefinitions={[
					{ id: "key", header: "Access key ID", cell: (r) => r.AccessKeyId },
					{ id: "status", header: "Status", cell: (r) => r.Status },
					{
						id: "created",
						header: "Created",
						cell: (r) =>
							r.CreateDate ? new Date(r.CreateDate).toLocaleString() : "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={() =>
										open({
											title:
												r.Status === "Active"
													? "Deactivate access key"
													: "Activate access key",
											confirm:
												r.Status === "Active" ? "Deactivate" : "Activate",
											description: `Change the status of ${r.AccessKeyId}?`,
											submit: async () => {
												await change("UpdateAccessKey", {
													AccessKeyId: r.AccessKeyId,
													Status: r.Status === "Active" ? "Inactive" : "Active",
												});
											},
										})
									}
								>
									{r.Status === "Active" ? "Deactivate" : "Activate"}
								</Button>
								<Button
									disabled={!enabled}
									onClick={() =>
										open({
											title: "Delete access key",
											confirm: "Delete",
											description: `Permanently delete ${r.AccessKeyId}? Applications using this key will lose access.`,
											submit: async () => {
												await change("DeleteAccessKey", {
													AccessKeyId: r.AccessKeyId,
												});
											},
										})
									}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<EmptyState
						title="No access keys"
						subtitle="Create an access key for programmatic access."
					/>
				}
			/>
		</SpaceBetween>
	);
}
