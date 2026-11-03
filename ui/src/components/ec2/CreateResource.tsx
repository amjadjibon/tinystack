import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { jsonObject, stringMap } from "#/lib/consoleActions";
import {
	base64,
	type Ec2Row,
	type Ec2Scope,
	type Ec2Section,
	ec2Call,
	ec2Kinds,
	object,
	parentVpc,
	rowId,
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

export function CreateResource({
	scope,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	onClose: () => void;
	onSaved: (id: string, result: Ec2Row) => void;
}) {
	const kind = ec2Kinds[scope.section];
	const sections: Ec2Section[] =
		scope.section === "endpoints"
			? ["vpcs", "subnets", "groups", "routes"]
			: scope.section === "nat"
				? ["vpcs", "subnets", "addresses"]
				: ["subnets", "groups", "routes", "acls"].includes(scope.section)
					? ["vpcs"]
					: [];
	const choices = useChoices(scope, sections);
	const [name, setName] = useState(""),
		[tags, setTags] = useState("{}");
	const [vpc, setVpc] = useState(scope.vpc ?? ""),
		[cidr, setCidr] = useState(
			scope.section === "subnets" ? "10.0.1.0/24" : "10.0.0.0/16",
		);
	const [zone, setZone] = useState(""),
		[publicIp, setPublicIp] = useState(false),
		[description, setDescription] = useState("");
	const [subnet, setSubnet] = useState(""),
		[allocation, setAllocation] = useState(""),
		[connectivity, setConnectivity] = useState("public");
	const [endpointType, setEndpointType] = useState("Gateway"),
		[service, setService] = useState(`com.amazonaws.${scope.region}.s3`);
	const [routeIds, setRouteIds] = useState<string[]>([]),
		[subnetIds, setSubnetIds] = useState<string[]>([]),
		[groupIds, setGroupIds] = useState<string[]>([]),
		[privateDns, setPrivateDns] = useState(false),
		[policy, setPolicy] = useState("");
	const [keyType, setKeyType] = useState("rsa"),
		[importKey, setImportKey] = useState(false),
		[publicKey, setPublicKey] = useState("");
	const [token] = useState(() => crypto.randomUUID()),
		result = useRef<Ec2Row>({});
	const needVpc = [
		"subnets",
		"groups",
		"routes",
		"acls",
		"nat",
		"endpoints",
	].includes(scope.section);
	const related = (section: "subnets" | "groups" | "routes") =>
		choices.items(section).filter((r) => parentVpc(r) === vpc);
	const requireValue = (value: string, label: string) => {
		if (!value) throw new Error(`Choose or enter ${label}.`);
		return value;
	};
	const savedId = () => {
		const nested: Record<string, string> = {
			vpcs: "Vpc",
			subnets: "Subnet",
			routes: "RouteTable",
			gateways: "InternetGateway",
			acls: "NetworkAcl",
			nat: "NatGateway",
			endpoints: "VpcEndpoint",
		};
		return rowId(
			scope.section,
			nested[scope.section]
				? object(result.current, nested[scope.section])
				: result.current,
		);
	};
	return (
		<ActionDialog
			title={
				scope.section === "addresses"
					? "Allocate Elastic IP address"
					: scope.section === "keys" && importKey
						? "Import key pair"
						: `Create ${kind.singular === "VPC" ? "VPC" : kind.singular.toLowerCase()}`
			}
			submit={scope.section === "addresses" ? "Allocate" : "Create"}
			onClose={onClose}
			onSaved={() => onSaved(savedId(), result.current)}
			plan={async () => {
				const values = stringMap(tags, "Tags");
				if (name) values.Name = name;
				const input: Ec2Row = {
					TagSpecifications: tagSpecifications(scope.section, values),
				};
				let operation = kind.create ?? "";
				if (needVpc) {
					if (choices.loading || choices.error)
						throw new Error("Resource choices must finish loading.");
					requireValue(vpc, "a VPC");
				}
				switch (scope.section) {
					case "vpcs":
						input.CidrBlock = requireValue(cidr, "an IPv4 CIDR block");
						break;
					case "subnets":
						input.VpcId = vpc;
						input.CidrBlock = requireValue(cidr, "an IPv4 CIDR block");
						if (zone) input.AvailabilityZone = zone;
						break;
					case "groups":
						input.VpcId = vpc;
						input.GroupName = requireValue(name, "a security group name");
						input.Description = requireValue(description, "a description");
						break;
					case "routes":
					case "acls":
						input.VpcId = vpc;
						input.ClientToken = token;
						break;
					case "addresses":
						input.Domain = "vpc";
						break;
					case "nat":
						input.SubnetId = requireValue(subnet, "a subnet");
						input.ConnectivityType = connectivity;
						input.ClientToken = token;
						if (connectivity === "public")
							input.AllocationId = requireValue(
								allocation,
								"an unassociated Elastic IP address",
							);
						break;
					case "endpoints":
						input.VpcId = vpc;
						input.VpcEndpointType = endpointType;
						input.ServiceName = requireValue(
							service,
							"an endpoint service name",
						);
						input.ClientToken = token;
						if (policy.trim()) {
							jsonObject(policy, "Endpoint policy");
							input.PolicyDocument = policy;
						}
						if (endpointType === "Gateway") input.RouteTableIds = routeIds;
						else {
							if (!subnetIds.length)
								throw new Error("Choose at least one subnet.");
							input.SubnetIds = subnetIds;
							input.SecurityGroupIds = groupIds;
							input.PrivateDnsEnabled = privateDns;
						}
						break;
					case "keys":
						input.KeyName = requireValue(name, "a key pair name");
						if (importKey) {
							if (new TextEncoder().encode(publicKey).length > 16384)
								throw new Error("Public key must be at most 16 KiB.");
							input.PublicKeyMaterial = base64(
								requireValue(publicKey, "an OpenSSH public key"),
							);
							operation = "ImportKeyPair";
						} else {
							input.KeyType = keyType;
							input.KeyFormat = "pem";
						}
						break;
				}
				return [
					{
						label: operation,
						run: async () => {
							result.current = await ec2Call(scope, operation, input);
						},
					},
					...(scope.section === "subnets" && publicIp
						? [
								{
									label: "Enable public IPv4 assignment",
									run: () =>
										ec2Call(scope, "ModifySubnetAttribute", {
											SubnetId: savedId(),
											MapPublicIpOnLaunch: { Value: true },
										}),
								},
							]
						: []),
				];
			}}
		>
			<SpaceBetween size="l">
				{needVpc && <ChoiceStatus {...choices} />}
				<Field
					label={
						scope.section === "groups"
							? "Security group name"
							: scope.section === "keys"
								? "Key pair name"
								: "Name tag (optional)"
					}
					value={name}
					onChange={setName}
				/>
				{needVpc && (
					<Choice
						label="VPC"
						value={vpc}
						options={optionsFor("vpcs", choices.items("vpcs"))}
						onChange={(value) => {
							setVpc(value);
							setSubnet("");
							setSubnetIds([]);
							setGroupIds([]);
							setRouteIds([]);
						}}
					/>
				)}
				{["vpcs", "subnets"].includes(scope.section) && (
					<Field
						label="IPv4 CIDR block"
						value={cidr}
						onChange={setCidr}
						description={
							scope.section === "subnets"
								? "Must be inside the selected VPC and must not overlap another subnet."
								: "For example, 10.0.0.0/16."
						}
					/>
				)}
				{scope.section === "subnets" && (
					<SpaceBetween size="l">
						<Field
							label="Availability Zone (optional)"
							value={zone}
							onChange={setZone}
							description={`Leave empty for the first local Availability Zone in ${scope.region}.`}
						/>
						<Checkbox
							checked={publicIp}
							onChange={({ detail }) => setPublicIp(detail.checked)}
						>
							Auto-assign public IPv4 addresses
						</Checkbox>
					</SpaceBetween>
				)}
				{scope.section === "groups" && (
					<Field
						label="Description"
						value={description}
						onChange={setDescription}
					/>
				)}
				{scope.section === "nat" && (
					<SpaceBetween size="l">
						<Choice
							label="Subnet"
							value={subnet}
							options={optionsFor("subnets", related("subnets"))}
							onChange={setSubnet}
						/>
						<Choice
							label="Connectivity type"
							value={connectivity}
							options={[
								{ label: "Public", value: "public" },
								{ label: "Private", value: "private" },
							]}
							onChange={setConnectivity}
						/>
						{connectivity === "public" && (
							<Choice
								label="Elastic IP allocation"
								value={allocation}
								options={optionsFor(
									"addresses",
									choices
										.items("addresses")
										.filter(
											(r) =>
												!text(r, "AssociationId") &&
												!text(r, "NetworkInterfaceId"),
										),
								)}
								onChange={setAllocation}
							/>
						)}
					</SpaceBetween>
				)}
				{scope.section === "endpoints" && (
					<SpaceBetween size="l">
						<Choice
							label="Endpoint type"
							value={endpointType}
							options={[
								{ label: "Gateway", value: "Gateway" },
								{ label: "Interface", value: "Interface" },
							]}
							onChange={setEndpointType}
						/>
						<Field
							label="Service name"
							value={service}
							onChange={setService}
							description={`com.amazonaws.${scope.region}.s3, dynamodb, or an implemented interface service such as sqs.`}
						/>
						{endpointType === "Gateway" ? (
							<Choices
								label="Route tables"
								value={routeIds}
								options={optionsFor("routes", related("routes"))}
								onChange={setRouteIds}
							/>
						) : (
							<SpaceBetween size="l">
								<Choices
									label="Subnets"
									value={subnetIds}
									options={optionsFor("subnets", related("subnets"))}
									onChange={setSubnetIds}
								/>
								<Choices
									label="Security groups"
									value={groupIds}
									options={optionsFor("groups", related("groups"))}
									onChange={setGroupIds}
								/>
								<Checkbox
									checked={privateDns}
									onChange={({ detail }) => setPrivateDns(detail.checked)}
								>
									Enable private DNS
								</Checkbox>
							</SpaceBetween>
						)}
						<Field
							label="Endpoint policy (JSON, optional)"
							value={policy}
							onChange={setPolicy}
							multiline
							description="Leave empty for the default policy. Endpoint policies are stored as metadata."
						/>
					</SpaceBetween>
				)}
				{scope.section === "keys" && (
					<SpaceBetween size="l">
						<Checkbox
							checked={importKey}
							onChange={({ detail }) => setImportKey(detail.checked)}
						>
							Import an existing public key
						</Checkbox>
						{importKey ? (
							<Field
								label="OpenSSH public key"
								value={publicKey}
								onChange={setPublicKey}
								multiline
							/>
						) : (
							<SpaceBetween size="l">
								<Choice
									label="Key type"
									value={keyType}
									options={[
										{ label: "RSA", value: "rsa" },
										{ label: "ED25519", value: "ed25519" },
									]}
									onChange={setKeyType}
								/>
								<Alert type="info">
									The private key is returned once after creation. Save it
									before closing the result.
								</Alert>
							</SpaceBetween>
						)}
					</SpaceBetween>
				)}
				<Field
					label="Tags (JSON)"
					value={tags}
					onChange={setTags}
					multiline
					description="Object of string values. The name above is stored as the Name tag."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
