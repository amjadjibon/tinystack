import {
	Alert,
	Box,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Toggle,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { type HostedZone, type Route53Scope, route53Call } from "#/lib/route53";
import { bareZoneId } from "#/lib/route53Helpers";

export function CreateZoneDialog({
	scope,
	vpcs,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	vpcs: Resource[];
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [name, setName] = useState(""),
		[comment, setComment] = useState(""),
		[isPrivate, setPrivate] = useState(false),
		[vpc, setVpc] = useState("");
	const options = vpcs.map((r) => ({
		label: `${r.arn.split("/").at(-1) ?? r.name} (${r.region})`,
		value: `${r.region}/${r.arn.split("/").at(-1) ?? r.name}`,
	}));
	const created = useRef("");
	return (
		<ActionDialog
			title="Create hosted zone"
			submit="Create hosted zone"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				if (!name.trim()) throw new Error("Enter a domain name.");
				if (isPrivate && !vpc)
					throw new Error("A private hosted zone needs a VPC.");
				const [region, id] = vpc.split("/");
				return [
					{
						label: "Create hosted zone",
						run: async () => {
							const out = await route53Call<{ HostedZone: HostedZone }>(
								scope,
								"CreateHostedZone",
								{
									Name: name.trim(),
									CallerReference: `console-${Date.now()}-${Math.random().toString(36).slice(2)}`,
									HostedZoneConfig: {
										...(comment ? { Comment: comment } : {}),
										PrivateZone: isPrivate,
									},
									...(isPrivate
										? { VPC: { VPCRegion: region, VPCId: id } }
										: {}),
								},
							);
							created.current = bareZoneId(out.HostedZone.Id);
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField label="Domain name" description="For example example.com.">
					<Input
						ariaLabel="Domain name"
						value={name}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<FormField
					label="Comment"
					description="Optional, up to 256 characters."
				>
					<Input
						ariaLabel="Comment"
						value={comment}
						onChange={({ detail }) => setComment(detail.value)}
					/>
				</FormField>
				<Toggle
					checked={isPrivate}
					onChange={({ detail }) => setPrivate(detail.checked)}
				>
					Private hosted zone
				</Toggle>
				{isPrivate && (
					<FormField label="VPC" description="The VPC that resolves this zone.">
						<Select
							ariaLabel="VPC"
							placeholder="Choose a VPC"
							empty="No VPCs found. Create one in EC2 first."
							selectedOption={options.find((o) => o.value === vpc) ?? null}
							options={options}
							onChange={({ detail }) =>
								setVpc(detail.selectedOption.value ?? "")
							}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditCommentDialog({
	scope,
	zone,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	zone: HostedZone;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [comment, setComment] = useState(zone.Config?.Comment ?? "");
	return (
		<ActionDialog
			title="Edit hosted zone comment"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "Update comment",
					run: () =>
						route53Call(scope, "UpdateHostedZoneComment", {
							Id: bareZoneId(zone.Id),
							Comment: comment,
						}),
				},
			]}
		>
			<FormField label="Comment" description="Up to 256 characters.">
				<Input
					ariaLabel="Comment"
					value={comment}
					onChange={({ detail }) => setComment(detail.value)}
				/>
			</FormField>
		</ActionDialog>
	);
}

export function DeleteZoneDialog({
	scope,
	zone,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	zone: HostedZone;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete hosted zone"
			submit="Delete hosted zone"
			confirmation={zone.Name}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "Delete hosted zone",
					run: () =>
						route53Call(scope, "DeleteHostedZone", { Id: bareZoneId(zone.Id) }),
				},
			]}
		>
			<SpaceBetween size="m">
				<Alert type="warning" header="This cannot be undone">
					Delete the hosted zone <strong>{zone.Name}</strong>. A zone can be
					deleted only after every record set except its own SOA and NS records
					has been removed.
				</Alert>
				<Box>
					The zone currently has {zone.ResourceRecordSetCount ?? 0} record sets.
				</Box>
			</SpaceBetween>
		</ActionDialog>
	);
}
