import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	Modal,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { EmptyState } from "#/components/EmptyState";
import { iamCall, iamInput, iamPages, policyText } from "#/lib/iam";
import { usePoll } from "#/lib/usePoll";
import type { IAMActions } from "./IAMForm";
export function IAMPolicyDocument({
	resource,
	enabled,
	run,
	open,
}: IAMActions) {
	return (
		<Container
			header={
				<Header
					variant="h2"
					description={`Default version: ${resource.details?.DefaultVersionId ?? "—"}`}
					actions={
						<Button
							disabled={!enabled}
							onClick={() =>
								open({
									title: "Edit permissions policy",
									description:
										"Saving creates a new policy version and sets it as the default.",
									fields: [
										{
											id: "Document",
											label: "Permissions policy",
											type: "json",
											required: true,
										},
									],
									values: { Document: policyText(resource.details?.Document) },
									submit: async (values) => {
										await run("CreatePolicyVersion", {
											PolicyDocument: values.Document,
											SetAsDefault: true,
										});
									},
								})
							}
						>
							Edit policy
						</Button>
					}
				>
					Permissions policy
				</Header>
			}
		>
			<CodeBlock text={policyText(resource.details?.Document)} />
		</Container>
	);
}

type PolicyVersion = {
	VersionId: string;
	IsDefaultVersion: boolean;
	CreateDate: string;
};
export function IAMPolicyVersions({
	resource,
	enabled,
	run,
	open,
	onError,
}: IAMActions & { onError: (error: string) => void }) {
	const [document, setDocument] = useState<{ id: string; text: string }>();
	const versions = usePoll(() =>
		enabled
			? iamPages<PolicyVersion>(
					resource.account,
					"ListPolicyVersions",
					iamInput(resource),
					"Versions",
				)
			: Promise.resolve(
					((resource.details?.Versions as string[] | undefined) ?? []).map(
						(VersionId) => ({
							VersionId,
							IsDefaultVersion:
								VersionId === resource.details?.DefaultVersionId,
							CreateDate: "",
						}),
					),
				),
	);
	const change = async (operation: string, VersionId: string) => {
		try {
			await run(operation, { VersionId });
		} finally {
			versions.reload();
		}
	};
	return (
		<SpaceBetween size="l">
			{versions.error && <Alert type="error">{versions.error}</Alert>}
			<Table
				items={versions.data ?? []}
				trackBy="VersionId"
				loading={versions.loading}
				header={<Header variant="h2">Policy versions</Header>}
				columnDefinitions={[
					{
						id: "version",
						header: "Version",
						cell: (r) => (
							<Button
								variant="inline-link"
								disabled={!enabled}
								onClick={async () => {
									try {
										const result = await iamCall<{
											PolicyVersion: { Document: string };
										}>(resource.account, "GetPolicyVersion", {
											...iamInput(resource),
											VersionId: r.VersionId,
										});
										setDocument({
											id: r.VersionId,
											text: policyText(result.PolicyVersion.Document),
										});
									} catch (err) {
										onError(err instanceof Error ? err.message : String(err));
									}
								}}
							>
								{r.VersionId}
							</Button>
						),
					},
					{
						id: "default",
						header: "Default",
						cell: (r) => (r.IsDefaultVersion ? "Yes" : "No"),
					},
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
									disabled={!enabled || r.IsDefaultVersion}
									onClick={() =>
										open({
											title: "Set default policy version",
											confirm: "Set as default",
											description: `Use ${r.VersionId} as the default policy version?`,
											submit: async () =>
												change("SetDefaultPolicyVersion", r.VersionId),
										})
									}
								>
									Set as default
								</Button>
								<Button
									disabled={!enabled || r.IsDefaultVersion}
									onClick={() =>
										open({
											title: "Delete policy version",
											confirm: "Delete",
											description: `Delete ${r.VersionId}?`,
											submit: async () =>
												change("DeletePolicyVersion", r.VersionId),
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
						title="No policy versions"
						subtitle="Refresh to load policy versions."
					/>
				}
			/>
			{document && (
				<Modal
					visible
					size="large"
					header={`Policy version ${document.id}`}
					onDismiss={() => setDocument(undefined)}
					footer={
						<Box float="right">
							<Button onClick={() => setDocument(undefined)}>Close</Button>
						</Box>
					}
				>
					<CodeBlock text={document.text} />
				</Modal>
			)}
		</SpaceBetween>
	);
}
