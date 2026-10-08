import {
	Alert,
	Button,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { EmptyState } from "#/components/EmptyState";
import type { Resource } from "#/lib/api";
import { iamSequence } from "#/lib/iam";
import type { IAMActions } from "./IAMForm";

export function IAMProfiles({
	resource,
	resources,
	enabled,
	run,
	open,
}: IAMActions & { resources: Resource[] }) {
	const profiles = resources.filter((r) => r.type === "instance-profile");
	const attached = profiles.filter(
		(r) =>
			String(r.details?.Role ?? "").toLowerCase() ===
			resource.name.toLowerCase(),
	);
	const available = profiles.filter((r) => !r.details?.Role);
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				An instance profile passes this role to EC2. Users and groups receive
				permission policies; trusted identities assume roles. Assign this
				profile when configuring an EC2 instance.
			</Alert>
			<Table
				items={attached}
				trackBy="arn"
				header={
					<Header
						variant="h2"
						counter={`(${attached.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled || !available.length}
									onClick={() => {
										const completed = new Set<string>();
										open({
											title: "Add role to instance profiles",
											confirm: "Add role",
											completed,
											fields: [
												{
													id: "Profiles",
													label: "Instance profiles",
													type: "multi",
													required: true,
													options: available.map((r) => ({
														label: r.name,
														value: r.name,
														description: r.arn,
													})),
												},
											],
											submit: async (values) => {
												await iamSequence(
													(JSON.parse(values.Profiles) as string[]).map(
														(name) => ({
															id: name,
															label: `Add role to ${name}`,
															run: () =>
																run("AddRoleToInstanceProfile", {
																	InstanceProfileName: name,
																	RoleName: resource.name,
																}),
														}),
													),
													completed,
												);
											},
										});
									}}
								>
									Add to existing profile
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										const completed = new Set<string>();
										open({
											title: "Create instance profile",
											confirm: "Create profile",
											completed,
											fields: [
												{
													id: "Name",
													label: "Instance profile name",
													required: true,
												},
												{ id: "Path", label: "Path", required: true },
											],
											values: { Name: resource.name, Path: "/" },
											submit: async (values) => {
												await iamSequence(
													[
														{
															id: "create",
															label: "Create instance profile",
															run: () =>
																run("CreateInstanceProfile", {
																	InstanceProfileName: values.Name,
																	Path: values.Path,
																}),
														},
														{
															id: "attach",
															label: "Add role to instance profile",
															run: () =>
																run("AddRoleToInstanceProfile", {
																	InstanceProfileName: values.Name,
																	RoleName: resource.name,
																}),
														},
													],
													completed,
												);
											},
										});
									}}
								>
									Create instance profile
								</Button>
							</SpaceBetween>
						}
					>
						Instance profiles
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Name", cell: (r) => r.name },
					{ id: "arn", header: "ARN", cell: (r) => r.arn },
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={() =>
										open({
											title: "Remove role from instance profile",
											confirm: "Remove role",
											description: `Remove ${resource.name} from ${r.name}? The empty profile will remain.`,
											submit: async () => {
												await run("RemoveRoleFromInstanceProfile", {
													InstanceProfileName: r.name,
													RoleName: resource.name,
												});
											},
										})
									}
								>
									Remove role
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										const completed = new Set<string>();
										open({
											title: "Delete instance profile",
											confirm: "Delete profile",
											description: `Remove the role and delete ${r.name}? This does not delete the role.`,
											completed,
											submit: async () => {
												await iamSequence(
													[
														{
															id: "remove",
															label: "Remove role",
															run: () =>
																run("RemoveRoleFromInstanceProfile", {
																	InstanceProfileName: r.name,
																	RoleName: resource.name,
																}),
														},
														{
															id: "delete",
															label: "Delete instance profile",
															run: () =>
																run("DeleteInstanceProfile", {
																	InstanceProfileName: r.name,
																}),
														},
													],
													completed,
												);
											},
										});
									}}
								>
									Delete profile
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<EmptyState
						title="No instance profiles"
						subtitle="Create an instance profile to use this role with EC2."
					/>
				}
			/>
		</SpaceBetween>
	);
}
