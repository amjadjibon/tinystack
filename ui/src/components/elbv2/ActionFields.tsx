import {
	Button,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { Pick, Text } from "#/components/memorydb/Fields";
import type { TargetGroup } from "#/lib/elbv2";
import {
	type ActionForm,
	actionTypes,
	type ConditionForm,
	conditionFields,
	emptyCondition,
	fixedContentTypes,
	redirectCodes,
} from "#/lib/elbv2RuleHelpers";

const valueHelp: Record<string, string> = {
	"path-pattern": "One path pattern per line, for example /api/*.",
	"host-header": "One host name per line, for example app.example.com.",
	"http-header": "One header value per line.",
	"http-request-method": "One method per line, for example GET.",
	"query-string": "One key=value pair per line; omit the key to match a value.",
	"source-ip": "One CIDR block per line, for example 10.0.0.0/8.",
};

export function ActionFields({
	form,
	groups,
	onChange,
}: {
	form: ActionForm;
	groups: TargetGroup[];
	onChange: (patch: Partial<ActionForm>) => void;
}) {
	return (
		<SpaceBetween size="m">
			<Pick
				label="Action type"
				value={form.type}
				placeholder="Choose an action"
				options={actionTypes}
				onChange={(type) => onChange({ type })}
			/>
			{form.type === "forward" && (
				<Pick
					label="Target group"
					value={form.targetGroup}
					placeholder="Choose a target group"
					description="The target group protocol must suit the listener protocol."
					options={groups.map((g) => ({
						value: g.TargetGroupArn,
						label: `${g.TargetGroupName} (${g.Protocol ?? g.TargetType})`,
					}))}
					onChange={(targetGroup) => onChange({ targetGroup })}
				/>
			)}
			{form.type === "fixed-response" && (
				<>
					<Text
						label="Response status code"
						value={form.fixedStatus}
						constraint="200 to 599."
						onChange={(fixedStatus) => onChange({ fixedStatus })}
					/>
					<Pick
						label="Content type"
						value={form.fixedType}
						placeholder="Choose a content type"
						options={fixedContentTypes.map((value) => ({ value }))}
						onChange={(fixedType) => onChange({ fixedType })}
					/>
					<FormField
						label="Response body"
						constraintText="Up to 1024 characters."
					>
						<Textarea
							ariaLabel="Response body"
							rows={3}
							value={form.fixedBody}
							onChange={({ detail }) => onChange({ fixedBody: detail.value })}
						/>
					</FormField>
				</>
			)}
			{form.type === "redirect" && (
				<>
					<Pick
						label="Redirect status"
						value={form.redirectCode}
						placeholder="Choose a status"
						options={redirectCodes.map((value) => ({ value }))}
						onChange={(redirectCode) => onChange({ redirectCode })}
					/>
					<Text
						label="Protocol"
						value={form.redirectProtocol}
						onChange={(redirectProtocol) => onChange({ redirectProtocol })}
					/>
					<Text
						label="Port"
						value={form.redirectPort}
						onChange={(redirectPort) => onChange({ redirectPort })}
					/>
					<Text
						label="Host"
						value={form.redirectHost}
						onChange={(redirectHost) => onChange({ redirectHost })}
					/>
					<Text
						label="Path"
						value={form.redirectPath}
						onChange={(redirectPath) => onChange({ redirectPath })}
					/>
					<Text
						label="Query"
						value={form.redirectQuery}
						onChange={(redirectQuery) => onChange({ redirectQuery })}
					/>
				</>
			)}
		</SpaceBetween>
	);
}

export function ConditionFields({
	forms,
	onChange,
}: {
	forms: ConditionForm[];
	onChange: (forms: ConditionForm[]) => void;
}) {
	const set = (i: number, patch: Partial<ConditionForm>) =>
		onChange(forms.map((f, j) => (j === i ? { ...f, ...patch } : f)));
	return (
		<SpaceBetween size="m">
			{forms.map((f, i) => (
				// biome-ignore lint/suspicious/noArrayIndexKey: editable rows have no identity.
				<SpaceBetween key={`${i}`} size="xs">
					<Pick
						label={`Condition ${i + 1} type`}
						value={f.field}
						placeholder="Choose a condition"
						options={conditionFields}
						onChange={(field) => set(i, { field })}
					/>
					{f.field === "http-header" && (
						<Text
							label={`Condition ${i + 1} header name`}
							value={f.headerName}
							onChange={(headerName) => set(i, { headerName })}
						/>
					)}
					<FormField
						label={`Condition ${i + 1} values`}
						description={valueHelp[f.field]}
					>
						<Textarea
							ariaLabel={`Condition ${i + 1} values`}
							rows={2}
							value={f.values}
							onChange={({ detail }) => set(i, { values: detail.value })}
						/>
					</FormField>
					{forms.length > 1 && (
						<Button onClick={() => onChange(forms.filter((_, j) => j !== i))}>
							Remove condition
						</Button>
					)}
				</SpaceBetween>
			))}
			<Button
				onClick={() =>
					onChange([...forms, { ...emptyCondition, field: "host-header" }])
				}
			>
				Add condition
			</Button>
		</SpaceBetween>
	);
}
