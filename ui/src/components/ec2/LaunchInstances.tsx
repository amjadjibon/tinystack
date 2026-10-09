import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { integer, stringMap } from "#/lib/consoleActions";
import {
	base64,
	type Ec2Row,
	type Ec2Scope,
	ec2Call,
	parentVpc,
	rowId,
	rows,
	tagSpecifications,
	text,
} from "#/lib/ec2";
import {
	Choice,
	ChoiceStatus,
	Choices,
	Field,
	optionsFor,
	useChoices,
} from "./Fields";
export function LaunchInstances({
	scope,
	resources,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	resources: Resource[];
	onClose: () => void;
	onSaved: (id: string, count: number) => void;
}) {
	const choices = useChoices(scope, [
		"images",
		"vpcs",
		"subnets",
		"groups",
		"keys",
	]);
	const [name, setName] = useState(""),
		[image, setImage] = useState(""),
		[type, setType] = useState("t3.micro"),
		[count, setCount] = useState("1"),
		[vpc, setVpc] = useState(scope.vpc ?? ""),
		[subnet, setSubnet] = useState(""),
		[groups, setGroups] = useState<string[]>([]),
		[key, setKey] = useState(""),
		[profile, setProfile] = useState(""),
		[privateIp, setPrivateIp] = useState(""),
		[userData, setUserData] = useState(""),
		[tags, setTags] = useState("{}"),
		[monitoring, setMonitoring] = useState(false);
	const [token] = useState(() => crypto.randomUUID()),
		result = useRef<Ec2Row>({});
	const profiles = resources.filter(
		(r) =>
			r.service === "iam" &&
			r.type === "instance-profile" &&
			r.account === scope.account,
	);
	const selectedImage = choices
		.items("images")
		.find((r) => rowId("images", r) === image);
	return (
		<ActionDialog
			title="Launch instances"
			submit="Launch instances"
			onClose={onClose}
			onSaved={() =>
				onSaved(
					text(rows(result.current, "Instances")[0] ?? {}, "InstanceId"),
					rows(result.current, "Instances").length,
				)
			}
			plan={async () => {
				if (choices.loading || choices.error)
					throw new Error("Resource choices must finish loading.");
				const n = integer(count, "Number of instances", 1, 20);
				if (!image || !vpc || !subnet)
					throw new Error("Choose an AMI, VPC, and subnet.");
				const tagMap = stringMap(tags, "Tags");
				if (name) tagMap.Name = name;
				if (new TextEncoder().encode(userData).length > 16384)
					throw new Error("User data must be at most 16 KiB.");
				const input: Ec2Row = {
					ImageId: image,
					InstanceType: type,
					MinCount: n,
					MaxCount: n,
					SubnetId: subnet,
					ClientToken: token,
					TagSpecifications: tagSpecifications("instances", tagMap),
					Monitoring: { Enabled: monitoring },
				};
				if (groups.length) input.SecurityGroupIds = groups;
				if (key) input.KeyName = key;
				if (profile) input.IamInstanceProfile = { Arn: profile };
				if (privateIp) input.PrivateIpAddress = privateIp;
				if (userData) input.UserData = base64(userData);
				return [
					{
						label: "Launch instances",
						run: async () => {
							result.current = await ec2Call(scope, "RunInstances", input);
						},
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Alert type="info">
					TinyStack models instance records and lifecycle states. No virtual
					machine boots, SSH session connects, or user data runs.
				</Alert>
				<ChoiceStatus {...choices} />
				<Field label="Name tag (optional)" value={name} onChange={setName} />
				<Choice
					label="Amazon Machine Image (AMI)"
					value={image}
					options={choices.items("images").map((r) => ({
						label: `${text(r, "Name")} (${text(r, "Architecture")})`,
						value: rowId("images", r),
					}))}
					onChange={(value) => {
						setImage(value);
						setType(
							text(
								choices
									.items("images")
									.find((r) => rowId("images", r) === value) ?? {},
								"Architecture",
							) === "arm64"
								? "t4g.micro"
								: "t3.micro",
						);
					}}
					description="Choose from TinyStack's fixed regional image catalogue."
				/>
				<Field
					label="Instance type"
					value={type}
					onChange={setType}
					description={`For example, ${text(selectedImage ?? {}, "Architecture") === "arm64" ? "t4g.micro" : "t3.micro"}. Architecture must match the AMI.`}
				/>
				<Field
					label="Number of instances"
					value={count}
					onChange={setCount}
					description="1–20; all requested instances must fit within the local account/Region limit."
				/>
				<Choice
					label="VPC"
					value={vpc}
					options={optionsFor("vpcs", choices.items("vpcs"))}
					onChange={(value) => {
						setVpc(value);
						setSubnet("");
						setGroups([]);
					}}
				/>
				<Choice
					label="Subnet"
					value={subnet}
					options={optionsFor(
						"subnets",
						choices.items("subnets").filter((r) => parentVpc(r) === vpc),
					)}
					onChange={setSubnet}
				/>
				<Choices
					label="Security groups"
					value={groups}
					options={optionsFor(
						"groups",
						choices.items("groups").filter((r) => parentVpc(r) === vpc),
					)}
					onChange={setGroups}
					description="Leave empty to use this VPC's default security group."
				/>
				<Choice
					label="Key pair (optional)"
					value={key}
					optional
					options={choices.items("keys").map((r) => ({
						label: text(r, "KeyName"),
						value: text(r, "KeyName"),
					}))}
					onChange={setKey}
				/>
				<Choice
					label="IAM instance profile (optional)"
					value={profile}
					optional
					options={profiles.map((r) => ({ label: r.name, value: r.arn }))}
					onChange={setProfile}
					description="Manage instance profiles under IAM roles."
				/>
				<Field
					label="Private IPv4 address (optional)"
					value={privateIp}
					onChange={setPrivateIp}
					description="Only available when launching one instance. Leave empty for automatic allocation."
				/>
				<Checkbox
					checked={monitoring}
					onChange={({ detail }) => setMonitoring(detail.checked)}
				>
					Enable detailed monitoring configuration
				</Checkbox>
				<Field
					label="User data (optional)"
					value={userData}
					onChange={setUserData}
					multiline
					description="Plain text, at most 16 KiB. Stored as metadata; not executed."
				/>
				<Field
					label="Tags (JSON)"
					value={tags}
					onChange={setTags}
					multiline
					description="Object of string values."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
