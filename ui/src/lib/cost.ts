import { api, type Resource } from "./api";

export type CostService =
	| "budgets"
	| "costexplorer"
	| "costandusagereportservice";
export type Definition = Record<string, unknown>;
export type BudgetNotification = {
	Notification: Definition;
	Subscribers: { SubscriptionType: string; Address: string }[];
};
export function costHref(
	service: CostService,
	account: string,
	section = "",
	name = "",
) {
	const search = new URLSearchParams({ account });
	if (section) search.set("section", section);
	if (name) search.set("name", name);
	return `/services/${service}?${search}`;
}
export function costCall(
	service: CostService,
	account: string,
	operation: string,
	input: Definition,
) {
	// These definitions are account-wide. Use a stable signing region, not the hidden regional scope.
	return api.action(service, operation, account, input, "us-east-1");
}
export function configuration(resource: Resource): Definition {
	const field =
		resource.type === "anomalymonitor"
			? "Monitor"
			: resource.type === "anomalysubscription"
				? "Subscription"
				: resource.type === "costcategory"
					? "Category"
					: "Configuration";
	return (resource.details?.[field] as Definition) ?? {};
}
export function notifications(resource: Resource): BudgetNotification[] {
	return (resource.details?.Notifications as BudgetNotification[]) ?? [];
}
export {
	budgetName,
	categoryName,
	jsonArray,
	notification,
	required,
	spend,
	subscriberAddresses,
} from "./costValidation";
export function period(value: unknown) {
	const p = value as { Start?: number; End?: number } | undefined;
	const date = (n?: number) =>
		n === undefined ? "—" : new Date(n * 1000).toISOString().slice(0, 10);
	return `${date(p?.Start)} – ${date(p?.End)}`;
}
