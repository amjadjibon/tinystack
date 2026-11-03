import {
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	Form,
	FormField,
	Header,
	Input,
	RadioGroup,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import { errorText, jsonObject, stringMap } from "#/lib/consoleActions";
import { type SNSScope, snsCall } from "#/lib/sns";
import { tagList } from "#/lib/tags";

export function SNSTopicForm({
	scope,
	topics,
	onCancel,
	onSaved,
}: {
	scope: SNSScope;
	topics: Resource[];
	onCancel: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState("");
	const [displayName, setDisplayName] = useState("");
	const [fifo, setFifo] = useState(false);
	const [dedup, setDedup] = useState(false);
	const [tags, setTags] = useState("{}");
	const [policy, setPolicy] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const save = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		try {
			if (
				!/^[A-Za-z0-9_-]{1,256}$/.test(name) &&
				!/^[A-Za-z0-9_-]{1,251}\.fifo$/.test(name)
			)
				throw new Error(
					"Use 1–256 letters, numbers, hyphens or underscores. FIFO topic names end with .fifo.",
				);
			if (fifo !== name.endsWith(".fifo"))
				throw new Error(
					"FIFO topic names must end with .fifo. Standard topics cannot use this suffix.",
				);
			if (topics.some((t) => t.name === name))
				throw new Error(
					"This topic already exists in the selected account and Region.",
				);
			if (displayName.length > 100)
				throw new Error("Display name must be at most 100 characters.");
			const attributes: Record<string, string> = {
				DisplayName: displayName,
				FifoTopic: String(fifo),
			};
			if (fifo) attributes.ContentBasedDeduplication = String(dedup);
			if (policy.trim()) {
				jsonObject(policy, "Access policy");
				attributes.Policy = policy.trim();
			}
			await snsCall(scope, "CreateTopic", {
				Name: name,
				Attributes: attributes,
				Tags: tagList(stringMap(tags, "Tags")),
			});
			onSaved(name);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<ContentLayout header={<Header variant="h1">Create topic</Header>}>
			<Form
				errorText={error}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onCancel}>
							Cancel
						</Button>
						<Button variant="primary" loading={busy} onClick={save}>
							Create topic
						</Button>
					</SpaceBetween>
				}
			>
				<fieldset disabled={busy} style={{ border: 0, margin: 0, padding: 0 }}>
					<SpaceBetween size="l">
						<Container header={<Header variant="h2">Details</Header>}>
							<SpaceBetween size="l">
								<FormField
									label="Type"
									description="The topic type cannot be changed after creation."
								>
									<RadioGroup
										value={fifo ? "fifo" : "standard"}
										onChange={({ detail }) => setFifo(detail.value === "fifo")}
										items={[
											{
												value: "standard",
												label: "Standard",
												description:
													"Deliver to SQS, Lambda, HTTP/HTTPS, email or SMS subscribers.",
											},
											{
												value: "fifo",
												label: "FIFO",
												description:
													"Ordered, deduplicated delivery to SQS queues.",
											},
										]}
									/>
								</FormField>
								<FormField
									label="Name"
									constraintText={
										fifo
											? "Include the .fifo suffix. Maximum 256 characters."
											: "Maximum 256 characters. Letters, numbers, hyphens and underscores."
									}
								>
									<Input
										ariaLabel="Topic name"
										value={name}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<FormField
									label="Display name — optional"
									constraintText="Maximum 100 characters"
								>
									<Input
										ariaLabel="Topic display name"
										value={displayName}
										onChange={({ detail }) => setDisplayName(detail.value)}
									/>
								</FormField>
								{fifo && (
									<Checkbox
										checked={dedup}
										onChange={({ detail }) => setDedup(detail.checked)}
									>
										Content-based deduplication
									</Checkbox>
								)}
								<Box>
									AWS account: {scope.account} · AWS Region: {scope.region}
								</Box>
							</SpaceBetween>
						</Container>
						<Container
							header={<Header variant="h2">Access policy — optional</Header>}
						>
							<FormField
								label="Access policy (JSON)"
								description="Leave empty to use the default owner policy. TinyStack stores policies without evaluating them."
							>
								<Textarea
									ariaLabel="Topic access policy"
									value={policy}
									rows={8}
									onChange={({ detail }) => setPolicy(detail.value)}
								/>
							</FormField>
						</Container>
						<Container header={<Header variant="h2">Tags — optional</Header>}>
							<FormField
								label="Tags (JSON)"
								description='Object of string values, for example {"team":"platform"}.'
							>
								<Textarea
									ariaLabel="Topic tags"
									value={tags}
									rows={4}
									onChange={({ detail }) => setTags(detail.value)}
								/>
							</FormField>
						</Container>
					</SpaceBetween>
				</fieldset>
			</Form>
		</ContentLayout>
	);
}
