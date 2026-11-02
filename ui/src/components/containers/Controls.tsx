import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Input,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { ConsoleLayout } from "#/components/Console";
import { useScope } from "#/lib/scope";
export function Field({
	label,
	value,
	onChange,
	multiline = false,
}: {
	label: string;
	value: string;
	onChange: (s: string) => void;
	multiline?: boolean;
}) {
	return (
		<FormField label={label}>
			{multiline ? (
				<Textarea
					value={value}
					rows={12}
					onChange={({ detail }) => onChange(detail.value)}
				/>
			) : (
				<Input
					value={value}
					onChange={({ detail }) => onChange(detail.value)}
				/>
			)}
		</FormField>
	);
}
export function Choice({
	label,
	value,
	options,
	onChange,
}: {
	label: string;
	value: string;
	options: string[];
	onChange: (s: string) => void;
}) {
	return (
		<FormField label={label}>
			<Select
				selectedOption={value ? { label: value, value } : null}
				options={options.map((value) => ({ label: value, value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
export function ContainerLayout({
	service,
	title,
	children,
}: {
	service: "ecr" | "ecs";
	title: string;
	children: ReactNode;
}) {
	const { canWrite, capabilities } = useScope();
	const href = `/services/${service}`;
	return (
		<ConsoleLayout
			activeHref={href}
			navigationHeader={{ text: title, href }}
			navigationItems={[
				{ type: "link", text: title, href },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[{ text: title, href }]}
			content={
				<SpaceBetween size="l">
					{capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data && !capabilities.data[service] && (
						<Alert type="warning">This service is not enabled.</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					{children}
				</SpaceBetween>
			}
		/>
	);
}
export function Rows<T>({
	title,
	items,
	columns,
	actions,
	loading = false,
	emptyText,
}: {
	title: string;
	items: T[];
	columns: {
		id: string;
		header: string;
		cell: (row: T) => ReactNode;
		// minWidth keeps a column of buttons from wrapping their text in a wide table.
		minWidth?: number;
	}[];
	actions?: ReactNode;
	loading?: boolean;
	// emptyText tells the reader how to add the first item, naming the action that does it.
	emptyText: string;
}) {
	const collection = useCollection(items, {
		filtering: {
			filteringFunction: (item, text) =>
				(typeof item === "string" ? item : JSON.stringify(item))
					.toLowerCase()
					.includes(text.toLowerCase()),
			empty: (
				<Box>
					No {title.toLowerCase()}. {emptyText}
				</Box>
			),
			noMatch: <Box>No matches. Clear the filter to see all items.</Box>,
		},
		pagination: { pageSize: 20 },
	});
	return (
		<Table
			{...collection.collectionProps}
			items={collection.items}
			loading={loading}
			columnDefinitions={columns}
			header={
				<Header counter={`(${items.length})`} actions={actions}>
					{title}
				</Header>
			}
			filter={
				<TextFilter
					{...collection.filterProps}
					filteringPlaceholder={`Find ${title.toLowerCase()}`}
				/>
			}
			pagination={<Pagination {...collection.paginationProps} />}
		/>
	);
}
export function Buttons({ children }: { children: ReactNode }) {
	return (
		<SpaceBetween direction="horizontal" size="xs">
			{children}
		</SpaceBetween>
	);
}
export function Action({
	children,
	onClick,
	disabled = false,
}: {
	children: ReactNode;
	onClick: () => void;
	disabled?: boolean;
}) {
	return (
		<Button disabled={disabled} onClick={onClick}>
			{children}
		</Button>
	);
}
