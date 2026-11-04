import {
	Alert,
	Button,
	ExpandableSection,
	FormField,
	Header,
	Input,
	Modal,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { adminFetch } from "#/lib/session";

type Fixture = {
	id: string;
	revision: string;
	lineCount: number;
	provenance: string;
	updatedAt: string;
};
const base = "/_tinystack/costs/fixtures";
async function request(path = "", method = "GET", body?: unknown) {
	const response = await adminFetch(`${base}${path}`, method, body);
	if (response.status === 403)
		throw new Error(
			"Cost data requires an administrator session. Sign in as an SSO administrator or connect root credentials.",
		);
	const result = response.status === 204 ? undefined : await response.json();
	if (!response.ok)
		throw new Error(result?.message ?? `Request failed (${response.status})`);
	return result;
}
export function CostData() {
	const [fixtures, setFixtures] = useState<Fixture[]>([]);
	const [id, setId] = useState("");
	const [editor, setEditor] = useState(
		'{\n  "provenance": "console",\n  "lines": []\n}',
	);
	const [error, setError] = useState("");
	const [notice, setNotice] = useState("");
	const [busy, setBusy] = useState(false);
	const [deleting, setDeleting] = useState<string>();

	async function load() {
		setFixtures((await request()).fixtures);
	}
	useEffect(() => {
		let current = true;
		request()
			.then((r) => {
				if (current) setFixtures(r.fixtures);
			})
			.catch((e) => {
				if (current) setError(String(e));
			});
		return () => {
			current = false;
		};
	}, []);
	async function run(action: () => Promise<void>) {
		setBusy(true);
		setError("");
		setNotice("");
		try {
			await action();
		} catch (e) {
			setError(String(e));
		} finally {
			setBusy(false);
		}
	}
	return (
		<SpaceBetween size="l">
			<Header variant="h1">Cost data</Header>
			<Alert>
				Fixtures belong to the server's default account, regardless of the
				account selected in the console. Different fixture IDs add together.
				Saving an existing ID replaces its complete dataset.
			</Alert>
			{!!error && <Alert type="error">{error}</Alert>}
			{!!notice && <Alert type="success">{notice}</Alert>}
			<Table
				items={fixtures}
				loading={busy}
				header={
					<Header
						variant="h2"
						actions={
							<Button onClick={() => run(load)}>Refresh fixtures</Button>
						}
					>
						Loaded fixtures
					</Header>
				}
				columnDefinitions={[
					{ id: "id", header: "ID", cell: (f) => f.id },
					{ id: "count", header: "Lines", cell: (f) => f.lineCount },
					{
						id: "provenance",
						header: "Provenance",
						cell: (f) => f.provenance || "—",
					},
					{ id: "updated", header: "Updated", cell: (f) => f.updatedAt },
					{
						id: "actions",
						header: "Actions",
						cell: (f) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={busy}
									onClick={() =>
										run(async () => {
											const data = await request(
												`/${encodeURIComponent(f.id)}`,
											);
											setId(f.id);
											setEditor(
												JSON.stringify(
													{ provenance: data.provenance, lines: data.lines },
													null,
													2,
												),
											);
										})
									}
								>
									View / replace
								</Button>
								<Button disabled={busy} onClick={() => setDeleting(f.id)}>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty="No fixtures loaded"
			/>
			<FormField
				label="Fixture ID"
				description="Letters, digits, underscores and hyphens; 1–128 characters."
			>
				<Input value={id} onChange={({ detail }) => setId(detail.value)} />
			</FormField>
			<FormField label="Load JSON file">
				<input
					aria-label="Load JSON file"
					type="file"
					accept=".json,application/json"
					onChange={(event) => {
						const file = event.target.files?.[0];
						if (file) void run(async () => setEditor(await file.text()));
					}}
				/>
			</FormField>
			<FormField
				label="Fixture JSON"
				description="Only provenance and lines are accepted. The server validates every line before replacing data."
			>
				<Textarea
					value={editor}
					rows={16}
					onChange={({ detail }) => setEditor(detail.value)}
				/>
			</FormField>
			<Button
				variant="primary"
				loading={busy}
				disabled={!/^[A-Za-z0-9_-]{1,128}$/.test(id)}
				onClick={() =>
					run(async () => {
						await request(
							`/${encodeURIComponent(id)}`,
							"PUT",
							JSON.parse(editor),
						);
						await load();
						setNotice(
							`Fixture ${id} saved. Refresh Cost and usage to query the new data.`,
						);
					})
				}
			>
				Upload / replace fixture
			</Button>
			<ExpandableSection headerText="Fixture format">
				<a
					href="https://amjadjibon.github.io/tinystack/guides/costs/"
					target="_blank"
					rel="noreferrer"
				>
					Costs guide and JSON schema
				</a>
			</ExpandableSection>
			{!!deleting && (
				<Modal
					visible
					header="Delete cost fixture"
					onDismiss={() => setDeleting(undefined)}
					footer={
						<SpaceBetween direction="horizontal" size="xs">
							<Button onClick={() => setDeleting(undefined)}>Cancel</Button>
							<Button
								variant="primary"
								loading={busy}
								onClick={() =>
									run(async () => {
										await request(`/${encodeURIComponent(deleting)}`, "DELETE");
										await load();
										setNotice(`Fixture ${deleting} deleted.`);
										setDeleting(undefined);
									})
								}
							>
								Delete fixture
							</Button>
						</SpaceBetween>
					}
				>
					Delete {deleting} and all its cost lines?
					{!!error && <Alert type="error">{error}</Alert>}
				</Modal>
			)}
		</SpaceBetween>
	);
}
