import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type OpenSearchScope, opensearchCall } from "#/lib/opensearch";
import {
	createDomainInput,
	type DomainForm,
	type DomainStatus,
	emptyDomainForm,
	formFromDomain,
	updateDomainInput,
} from "#/lib/opensearchHelpers";
import { DomainFormFields } from "./DomainFields";

export function CreateDomainDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: OpenSearchScope;
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [form, setForm] = useState(emptyDomainForm);
	const created = useRef("");
	const set = (patch: Partial<DomainForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create domain"
			submit="Create domain"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = createDomainInput(form);
				return [
					{
						label: "CreateDomain",
						run: async () => {
							const out = await opensearchCall<{
								DomainStatus?: DomainStatus;
							}>(scope, "CreateDomain", input);
							created.current =
								out.DomainStatus?.DomainName ?? input.DomainName;
						},
					},
				];
			}}
		>
			<DomainFormFields form={form} set={set} />
		</ActionDialog>
	);
}

export function EditDomainDialog({
	scope,
	domain,
	onClose,
	onSaved,
}: {
	scope: OpenSearchScope;
	domain: DomainStatus;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() => formFromDomain(domain));
	const set = (patch: Partial<DomainForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title={`Edit ${domain.DomainName}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = updateDomainInput(domain, form);
				return [
					{
						label: "UpdateDomainConfig",
						run: () => opensearchCall(scope, "UpdateDomainConfig", input),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Alert type="info">
					The name and the engine version cannot change: TinyStack does not
					implement upgrades. Tags are edited on the page.
				</Alert>
				<DomainFormFields form={form} set={set} domain={domain} />
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteDomainDialog({
	scope,
	domain,
	docker,
	onClose,
	onSaved,
}: {
	scope: OpenSearchScope;
	domain: DomainStatus;
	docker: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete domain"
			submit="Delete domain"
			confirmation={domain.DomainName}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteDomain",
					run: () =>
						opensearchCall(scope, "DeleteDomain", {
							DomainName: domain.DomainName,
						}),
				},
			]}
		>
			<Alert type="warning">
				Delete {domain.DomainName} and its tags permanently.
				{docker &&
					" Its engine container and data volume are removed too, so the indexed data is lost."}
			</Alert>
		</ActionDialog>
	);
}
