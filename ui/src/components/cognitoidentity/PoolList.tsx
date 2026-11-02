import {
	Alert,
	Button,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Buttons, Rows } from "#/components/containers/Controls";
import {
	type IdentityScope,
	identityHref,
	listIdentityPools,
} from "#/lib/cognitoidentity";
import {
	type IdentityPool,
	providerSummary,
} from "#/lib/cognitoidentityHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { Copy } from "./Field";
import { DeletePoolDialog, PoolDialog } from "./PoolDialog";

export function PoolList({
	scope,
	enabled,
	onChanged,
}: {
	scope: IdentityScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const follow = useFollow();
	const navigate = useNavigate();
	const poll = usePoll(() =>
		enabled ? listIdentityPools(scope) : Promise.resolve([]),
	);
	// editing is the pool the dialog edits; the dialog creates one when it is unset.
	const [dialog, setDialog] = useState<{ editing?: IdentityPool }>();
	const [deleting, setDeleting] = useState<IdentityPool>();
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Identity pools"
				emptyText="Choose Create identity pool to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Button onClick={poll.reload}>Refresh</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog({})}
						>
							Create identity pool
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (p) => (
							<Link
								href={identityHref(scope, p.IdentityPoolId)}
								onFollow={follow}
							>
								{p.IdentityPoolName}
							</Link>
						),
					},
					{
						id: "id",
						header: "Identity pool ID",
						cell: (p) => (
							<Copy text={p.IdentityPoolId} label="Identity pool ID" />
						),
					},
					{
						id: "unauthenticated",
						header: "Unauthenticated identities",
						cell: (p) =>
							p.AllowUnauthenticatedIdentities ? "Allowed" : "Not allowed",
					},
					{
						id: "providers",
						header: "Authentication providers",
						cell: providerSummary,
					},
					{
						id: "actions",
						header: "Actions",
						cell: (p) => (
							<Buttons>
								<Button
									disabled={!enabled}
									onClick={() => setDialog({ editing: p })}
								>
									Edit
								</Button>
								<Button disabled={!enabled} onClick={() => setDeleting(p)}>
									Delete
								</Button>
							</Buttons>
						),
					},
				]}
			/>
			{dialog && (
				<PoolDialog
					scope={scope}
					pool={dialog.editing}
					onClose={() => setDialog(undefined)}
					onSaved={(id) => {
						const created = !dialog.editing;
						setDialog(undefined);
						refresh();
						if (created) navigate({ href: identityHref(scope, id) });
					}}
				/>
			)}
			{deleting && (
				<DeletePoolDialog
					scope={scope}
					pool={deleting}
					onClose={() => setDeleting(undefined)}
					onDeleted={() => {
						setDeleting(undefined);
						refresh();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
