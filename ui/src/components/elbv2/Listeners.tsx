import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import {
	type ELBv2Scope,
	type Listener,
	type LoadBalancer,
	listListeners,
	listSslPolicies,
	type TargetGroup,
} from "#/lib/elbv2";
import { describeAction } from "#/lib/elbv2RuleHelpers";
import { usePoll } from "#/lib/usePoll";
import { ListenerDetails } from "./ListenerDetails";
import { DeleteListenerDialog, ListenerDialog } from "./ListenerDialogs";

type Dialog = "" | "create" | "edit" | "delete";

export function Listeners({
	scope,
	balancer,
	groups,
	enabled,
}: {
	scope: ELBv2Scope;
	balancer: LoadBalancer;
	groups: TargetGroup[];
	enabled: boolean;
}) {
	const poll = usePoll(() => listListeners(scope, balancer.LoadBalancerArn), 0);
	const policies = usePoll(() => listSslPolicies(scope), 0);
	const [selected, setSelected] = useState("");
	const [dialog, setDialog] = useState<Dialog>("");
	const listeners = poll.data ?? [];
	const current = listeners.find((l) => l.ListenerArn === selected);
	const names = (arn: string) =>
		groups.find((g) => g.TargetGroupArn === arn)?.TargetGroupName ?? arn;
	const saved = () => {
		setDialog("");
		poll.reload();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!!policies.error && <Alert type="error">{policies.error}</Alert>}
			<Rows
				title="Listeners"
				emptyText="Choose Create listener to accept traffic on a protocol and port."
				items={listeners}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => setDialog("create")}>
							Create listener
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "listener",
						header: "Listener",
						cell: (l: Listener) => (
							<Action onClick={() => setSelected(l.ListenerArn)}>
								{l.Protocol}:{l.Port}
							</Action>
						),
					},
					{
						id: "action",
						header: "Default action",
						cell: (l) =>
							(l.DefaultActions ?? [])
								.map((a) => describeAction(a, names))
								.join(", "),
					},
					{
						id: "policy",
						header: "SSL policy",
						cell: (l) => l.SslPolicy ?? "-",
					},
					{
						id: "manage",
						header: "Actions",
						cell: (l) => (
							<Buttons>
								<Action
									disabled={!enabled}
									onClick={() => {
										setSelected(l.ListenerArn);
										setDialog("edit");
									}}
								>
									Edit
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => {
										setSelected(l.ListenerArn);
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{current && (
				<ListenerDetails
					key={current.ListenerArn}
					scope={scope}
					listener={current}
					groups={groups}
					enabled={enabled}
					onChanged={poll.reload}
				/>
			)}
			{dialog === "create" && (
				<ListenerDialog
					scope={scope}
					balancer={balancer}
					groups={groups}
					policies={policies.data ?? []}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "edit" && current && (
				<ListenerDialog
					scope={scope}
					balancer={balancer}
					listener={current}
					groups={groups}
					policies={policies.data ?? []}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && current && (
				<DeleteListenerDialog
					scope={scope}
					listener={current}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelected("");
						saved();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
