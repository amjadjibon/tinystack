import {
	Alert,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import {
	type ELBv2Scope,
	type LoadBalancer,
	listLoadBalancers,
	type SecurityGroup,
	type Subnet,
	type TargetGroup,
} from "#/lib/elbv2";
import { hasPendingBalancers, isPendingState } from "#/lib/elbv2Helpers";
import { usePoll } from "#/lib/usePoll";
import { Listeners } from "./Listeners";
import {
	CreateLoadBalancerDialog,
	DeleteLoadBalancerDialog,
	EditNetworkDialog,
} from "./LoadBalancerDialogs";
import { AttributesDialog, Details, ResourceTags } from "./Shared";

function statusType(code?: string) {
	if (code === "active") return "success";
	if (isPendingState(code)) return "in-progress";
	return code === "failed" ? "error" : "info";
}

function Dns({ name }: { name?: string }) {
	return name ? (
		<CopyToClipboard
			variant="inline"
			textToCopy={name}
			copySuccessText="DNS name copied"
			copyErrorText="DNS name failed to copy"
		/>
	) : (
		"-"
	);
}

type Dialog = "" | "create" | "network" | "attributes" | "delete";

export function LoadBalancers({
	scope,
	enabled,
	subnets,
	securityGroups,
	groups,
	onChanged,
}: {
	scope: ELBv2Scope;
	enabled: boolean;
	subnets: Subnet[];
	securityGroups: SecurityGroup[];
	groups: TargetGroup[];
	onChanged: () => void;
}) {
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			const items = enabled ? await listLoadBalancers(scope) : [];
			setPending(hasPendingBalancers(items));
			return items;
		},
		pending ? 3000 : 0,
	);
	const [selected, setSelected] = useState("");
	const [dialog, setDialog] = useState<Dialog>("");
	const [target, setTarget] = useState<LoadBalancer>();
	const balancers = poll.data ?? [];
	const detail = balancers.find((b) => b.LoadBalancerName === selected);
	const open = (kind: Dialog, b?: LoadBalancer) => {
		setTarget(b);
		setDialog(kind);
	};
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	const dialogTarget = target ?? detail;
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Load balancers"
				emptyText="Choose Create load balancer to get started."
				items={balancers}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create load balancer
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (b) => (
							<Action onClick={() => setSelected(b.LoadBalancerName)}>
								{b.LoadBalancerName}
							</Action>
						),
					},
					{ id: "type", header: "Type", cell: (b) => b.Type },
					{ id: "scheme", header: "Scheme", cell: (b) => b.Scheme },
					{
						id: "state",
						header: "State",
						cell: (b) => (
							<StatusIndicator type={statusType(b.State?.Code)}>
								{b.State?.Code}
							</StatusIndicator>
						),
					},
					{
						id: "dns",
						header: "DNS name",
						cell: (b) => <Dns name={b.DNSName} />,
					},
					{ id: "vpc", header: "VPC", cell: (b) => b.VpcId },
					{
						id: "manage",
						header: "Actions",
						cell: (b) => (
							<Action disabled={!enabled} onClick={() => open("delete", b)}>
								Delete
							</Action>
						),
					},
				]}
			/>
			{detail && (
				<SpaceBetween size="l" key={detail.LoadBalancerArn}>
					<Container
						header={
							<Header
								variant="h2"
								description={detail.LoadBalancerArn}
								actions={
									<Buttons>
										<Action
											disabled={!enabled}
											onClick={() => open("network", detail)}
										>
											Edit subnets and security groups
										</Action>
										<Action
											disabled={!enabled}
											onClick={() => open("attributes", detail)}
										>
											Edit attributes
										</Action>
										<Action
											disabled={!enabled}
											onClick={() => open("delete", detail)}
										>
											Delete
										</Action>
									</Buttons>
								}
							>
								{detail.LoadBalancerName}
							</Header>
						}
					>
						<Details
							items={[
								["State", detail.State?.Code],
								["Type", detail.Type],
								["Scheme", detail.Scheme],
								["IP address type", detail.IpAddressType],
								["VPC", detail.VpcId],
								["DNS name", <Dns key="dns" name={detail.DNSName} />],
								[
									"Subnets",
									(detail.AvailabilityZones ?? [])
										.map((z) => `${z.SubnetId} (${z.ZoneName})`)
										.join(", "),
								],
								["Security groups", (detail.SecurityGroups ?? []).join(", ")],
							]}
						/>
					</Container>
					<Listeners
						scope={scope}
						balancer={detail}
						groups={groups}
						enabled={enabled}
					/>
					<ResourceTags
						scope={scope}
						arn={detail.LoadBalancerArn}
						enabled={enabled}
					/>
				</SpaceBetween>
			)}
			{dialog === "create" && (
				<CreateLoadBalancerDialog
					scope={scope}
					subnets={subnets}
					groups={securityGroups}
					onClose={() => setDialog("")}
					onSaved={(name) => {
						setSelected(name);
						changed();
					}}
				/>
			)}
			{dialog === "network" && dialogTarget && (
				<EditNetworkDialog
					scope={scope}
					balancer={dialogTarget}
					subnets={subnets}
					groups={securityGroups}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "attributes" && dialogTarget && (
				<AttributesDialog
					scope={scope}
					title={`Attributes of ${dialogTarget.LoadBalancerName}`}
					arn={dialogTarget.LoadBalancerArn}
					idKey="LoadBalancerArn"
					describe="DescribeLoadBalancerAttributes"
					modify="ModifyLoadBalancerAttributes"
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "delete" && dialogTarget && (
				<DeleteLoadBalancerDialog
					scope={scope}
					balancer={dialogTarget}
					onClose={() => setDialog("")}
					onSaved={() => {
						if (selected === dialogTarget.LoadBalancerName) setSelected("");
						changed();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
