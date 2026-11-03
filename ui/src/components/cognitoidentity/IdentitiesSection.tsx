import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	ExpandableSection,
	Header,
	Modal,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import {
	type IdentityScope,
	identityCall,
	listIdentities,
} from "#/lib/cognitoidentity";
import {
	callLimit,
	chunks,
	type Identity,
	identityKind,
} from "#/lib/cognitoidentityHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
import { Copy, Field } from "./Field";

function IdentityDetails({
	scope,
	id,
	onClose,
}: {
	scope: IdentityScope;
	id: string;
	onClose: () => void;
}) {
	const poll = usePoll(
		() => identityCall<Identity>(scope, "DescribeIdentity", { IdentityId: id }),
		0,
	);
	const i = poll.data;
	return (
		<Modal
			visible
			size="large"
			header="Identity"
			closeAriaLabel="Close identity details"
			onDismiss={onClose}
		>
			<SpaceBetween size="m">
				{poll.error && <Alert type="error">{poll.error}</Alert>}
				{!i && !poll.error && <Box>Loading identity…</Box>}
				{i && (
					<SpaceBetween size="m">
						<ColumnLayout columns={2} variant="text-grid">
							<Field label="Identity ID">
								<Copy text={i.IdentityId} label="Identity ID" />
							</Field>
							<Field label="Type">{identityKind(i)}</Field>
							<Field label="Logins">{i.Logins?.join(", ") || "-"}</Field>
							<Field label="Created">{unixTime(i.CreationDate)}</Field>
							<Field label="Last modified">
								{unixTime(i.LastModifiedDate)}
							</Field>
						</ColumnLayout>
						<ExpandableSection headerText="Raw JSON">
							<CodeBlock text={JSON.stringify(i, null, 2)} />
						</ExpandableSection>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Modal>
	);
}

export function IdentitiesSection({
	scope,
	pool,
	enabled,
	onChanged,
}: {
	scope: IdentityScope;
	pool: string;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(() =>
		enabled ? listIdentities(scope, pool) : Promise.resolve([]),
	);
	const [chosen, setChosen] = useState<string[]>([]);
	const [viewing, setViewing] = useState<string>();
	const [deleting, setDeleting] = useState(false);
	const identities = poll.data ?? [];
	const selected = identities.filter((i) => chosen.includes(i.IdentityId));
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(identities, {
			filtering: {
				filteringFunction: (i, text) =>
					[i.IdentityId, identityKind(i), ...(i.Logins ?? [])]
						.join(" ")
						.toLowerCase()
						.includes(text.toLowerCase()),
				empty: (
					<Box>
						No identities. Clients get one when they call GetId, or use the test
						below.
					</Box>
				),
				noMatch: <Box>No matches. Clear the filter to see all identities.</Box>,
			},
			sorting: {
				defaultState: {
					sortingColumn: { sortingField: "CreationDate" },
					isDescending: true,
				},
			},
			pagination: { pageSize: 20 },
		});
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="s">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				{...collectionProps}
				items={items}
				trackBy="IdentityId"
				loading={poll.loading}
				selectionType="multi"
				selectedItems={selected}
				onSelectionChange={({ detail }) =>
					setChosen(detail.selectedItems.map((i) => i.IdentityId))
				}
				ariaLabels={{
					selectionGroupLabel: "Select identities",
					itemSelectionLabel: (_, i) => `Select ${i.IdentityId}`,
					allItemsSelectionLabel: () => "Select all identities on this page",
				}}
				filter={
					<TextFilter {...filterProps} filteringPlaceholder="Find identities" />
				}
				pagination={<Pagination {...paginationProps} />}
				header={
					<Header
						variant="h2"
						counter={`(${identities.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={poll.reload}>Refresh</Button>
								<Button
									disabled={!enabled || !selected.length}
									onClick={() => setDeleting(true)}
								>
									Delete identities
								</Button>
							</SpaceBetween>
						}
					>
						Identities
					</Header>
				}
				columnDefinitions={[
					{
						id: "id",
						header: "Identity ID",
						sortingField: "IdentityId",
						cell: (i: Identity) => (
							<Button
								variant="inline-link"
								onClick={() => setViewing(i.IdentityId)}
							>
								{i.IdentityId}
							</Button>
						),
					},
					{ id: "type", header: "Type", cell: identityKind },
					{
						id: "logins",
						header: "Logins",
						cell: (i) => i.Logins?.join(", ") || "-",
					},
					{
						id: "created",
						header: "Created",
						sortingField: "CreationDate",
						cell: (i) => unixTime(i.CreationDate),
					},
					{
						id: "modified",
						header: "Last modified",
						sortingField: "LastModifiedDate",
						cell: (i) => unixTime(i.LastModifiedDate),
					},
				]}
			/>
			{viewing && (
				<IdentityDetails
					key={viewing}
					scope={scope}
					id={viewing}
					onClose={() => setViewing(undefined)}
				/>
			)}
			{deleting && (
				<ActionDialog
					title="Delete identities"
					submit="Delete identities"
					confirmation="delete"
					onClose={() => setDeleting(false)}
					onSaved={() => {
						setDeleting(false);
						setChosen([]);
						refresh();
					}}
					plan={async () =>
						chunks(
							selected.map((i) => i.IdentityId),
							callLimit,
						).map((ids) => ({
							label: "DeleteIdentities",
							run: async () => {
								const out = await identityCall<{
									UnprocessedIdentityIds?: {
										IdentityId?: string;
										ErrorCode?: string;
									}[];
								}>(scope, "DeleteIdentities", { IdentityIdsToDelete: ids });
								const failed = out.UnprocessedIdentityIds ?? [];
								if (failed.length)
									throw new Error(
										`Not deleted: ${failed.map((f) => `${f.IdentityId} (${f.ErrorCode})`).join(", ")}`,
									);
							},
						}))
					}
				>
					<Alert type="warning">
						Delete {selected.length}{" "}
						{selected.length === 1 ? "identity" : "identities"} permanently.
						Clients that still hold one have to call GetId again.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
