import {
	AppLayout,
	type AppLayoutProps,
	Autosuggest,
	Box,
	BreadcrumbGroup,
	type BreadcrumbGroupProps,
	Button,
	Flashbar,
	FormField,
	Modal,
	SideNavigation,
	type SideNavigationProps,
	SpaceBetween,
	TopNavigation,
	type TopNavigationProps,
} from "@cloudscape-design/components";
import { Density, Mode } from "@cloudscape-design/global-styles";
import { useLocation, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";
import { useHealth } from "#/lib/health";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { service } from "#/lib/services";
import {
	applyPreferences,
	loadPreferences,
	type Preferences,
} from "#/lib/theme";
import { useFollow } from "#/lib/useFollow";

const headerID = "console-header";

// ConsoleHeader is the top bar every page shares: the TinyStack identity,
// a service search, account/Region menus, and the display settings.
export function ConsoleHeader() {
	const navigate = useNavigate();
	const follow = useFollow();
	const { data } = useHealth();
	const [preferences, setPreferences] = useState<Preferences>(loadPreferences);
	const [query, setQuery] = useState("");
	const scope = useScope();
	const [scopeDialog, setScopeDialog] = useState<"account" | "region" | null>(
		null,
	);
	const [scopeDraft, setScopeDraft] = useState("");
	const openScopeDialog = (kind: "account" | "region") => {
		setScopeDraft(scope[kind]);
		setScopeDialog(kind);
	};
	const scopeUtilities: TopNavigationProps.Utility[] = [
		scope.global
			? {
					type: "button",
					text: "Global",
					ariaLabel: "Global service",
					title: "This service is global",
				}
			: {
					type: "menu-dropdown",
					text: scope.region || "Region",
					title: "AWS Region",
					ariaLabel: `AWS Region: ${scope.region || "Choose Region"}`,
					disableTextCollapse: true,
					items: [
						...scope.regions.map((region) => ({
							id: region,
							text: region,
							itemType: "checkbox" as const,
							checked: region === scope.region,
						})),
						{ id: "switch", text: "Switch Region…" },
					],
					onItemClick: ({ detail }) =>
						detail.id === "switch"
							? openScopeDialog("region")
							: scope.switchScope({ region: detail.id }),
				},
		{
			type: "menu-dropdown",
			text: scope.account || "Account",
			title: "AWS account",
			ariaLabel: `AWS account: ${scope.account || "Choose account"}`,
			disableTextCollapse: true,
			items: [
				...scope.accounts.map((account) => ({
					id: account,
					text: account,
					itemType: "checkbox" as const,
					checked: account === scope.account,
				})),
				{ id: "switch", text: "Switch account…" },
			],
			onItemClick: ({ detail }) =>
				detail.id === "switch"
					? openScopeDialog("account")
					: scope.switchScope({ account: detail.id }),
		},
	];
	const validDraft =
		scopeDialog === "account"
			? /^\d{12}$/.test(scopeDraft)
			: validRegion(scopeDraft);

	useEffect(() => applyPreferences(preferences), [preferences]);

	const matches = (data?.services ?? []).filter((name) =>
		`${name} ${service(name).title}`
			.toLowerCase()
			.includes(query.toLowerCase()),
	);

	return (
		<div id={headerID} style={{ position: "sticky", top: 0, zIndex: 1002 }}>
			<TopNavigation
				identity={{
					href: "/",
					title: "TinyStack",
					logo: { src: "/tinystack.svg", alt: "" },
					onFollow: follow,
				}}
				search={
					<ServiceSearch
						query={query}
						matches={matches}
						onChange={setQuery}
						onSelect={(name) => {
							setQuery("");
							navigate({ href: `/services/${name}` });
						}}
					/>
				}
				utilities={[
					{
						type: "button",
						text: data ? `${data.storage} storage` : "Connecting…",
						iconName: data ? "status-positive" : "status-pending",
						title: "TinyStack server status",
					},
					{
						type: "menu-dropdown",
						iconName: "settings",
						ariaLabel: "Display settings",
						title: "Display settings",
						items: [
							{
								id: "mode",
								text: "Theme",
								items: [
									{
										id: Mode.Light,
										text: "Light",
										checked: preferences.mode === Mode.Light,
										itemType: "checkbox",
									},
									{
										id: Mode.Dark,
										text: "Dark",
										checked: preferences.mode === Mode.Dark,
										itemType: "checkbox",
									},
								],
							},
							{
								id: "density",
								text: "Density",
								items: [
									{
										id: Density.Comfortable,
										text: "Comfortable",
										checked: preferences.density === Density.Comfortable,
										itemType: "checkbox",
									},
									{
										id: Density.Compact,
										text: "Compact",
										checked: preferences.density === Density.Compact,
										itemType: "checkbox",
									},
								],
							},
						],
						onItemClick: ({ detail }) => {
							if (detail.id === Mode.Light || detail.id === Mode.Dark) {
								setPreferences((p) => ({ ...p, mode: detail.id as Mode }));
							}

							if (
								detail.id === Density.Comfortable ||
								detail.id === Density.Compact
							) {
								setPreferences((p) => ({
									...p,
									density: detail.id as Density,
								}));
							}
						},
					},
					...scopeUtilities,
				]}
				i18nStrings={{
					overflowMenuTriggerText: "More",
					overflowMenuTitleText: "Console menu",
					overflowMenuBackIconAriaLabel: "Back",
					overflowMenuDismissIconAriaLabel: "Close menu",
				}}
			/>
			{scopeDialog && (
				<Modal
					visible
					header={
						scopeDialog === "account" ? "Switch account" : "Switch Region"
					}
					onDismiss={() => setScopeDialog(null)}
					footer={
						<Box float="right">
							<SpaceBetween direction="horizontal" size="xs">
								<Button variant="link" onClick={() => setScopeDialog(null)}>
									Cancel
								</Button>
								<Button
									variant="primary"
									disabled={!validDraft || scopeDraft === scope[scopeDialog]}
									onClick={() => {
										scope.switchScope({ [scopeDialog]: scopeDraft });
										setScopeDialog(null);
									}}
								>
									Switch {scopeDialog === "account" ? "account" : "Region"}
								</Button>
							</SpaceBetween>
						</Box>
					}
				>
					<FormField
						label={scopeDialog === "account" ? "AWS account" : "AWS Region"}
						errorText={
							scopeDraft && !validDraft
								? scopeDialog === "account"
									? "Enter a 12-digit account ID."
									: "Enter a Region such as us-east-1."
								: undefined
						}
					>
						<Autosuggest
							value={scopeDraft}
							options={(scopeDialog === "account"
								? scope.accounts
								: scope.regions
							).map((value) => ({ value }))}
							ariaLabel={
								scopeDialog === "account" ? "AWS account" : "AWS Region"
							}
							enteredTextLabel={(value) => `Use ${value}`}
							onChange={({ detail }) => setScopeDraft(detail.value)}
						/>
					</FormField>
				</Modal>
			)}
		</div>
	);
}

function ServiceSearch({
	query,
	matches,
	onChange,
	onSelect,
}: {
	query: string;
	matches: string[];
	onChange: (query: string) => void;
	onSelect: (name: string) => void;
}) {
	return (
		<Autosuggest
			value={query}
			onChange={({ detail }) => onChange(detail.value)}
			onSelect={({ detail }) => detail.value && onSelect(detail.value)}
			options={matches.map((name) => ({
				value: name,
				label: service(name).title,
				description: name,
				iconUrl: service(name).icon
					? `/aws-icons/${service(name).icon}.svg`
					: undefined,
			}))}
			placeholder="Search services"
			ariaLabel="Search services"
			enteredTextLabel={(value) => `Search "${value}"`}
			hideEnteredTextOption
			empty="No matching services"
			filteringType="manual"
		/>
	);
}

function navigation(services: string[]): SideNavigationProps.Item[] {
	return [
		{ type: "link", text: "Console Home", href: "/" },
		{ type: "link", text: "All resources", href: "/resources" },
		{ type: "divider" },
		{
			type: "section",
			text: "Monitoring",
			items: [
				{ type: "link", text: "Request inspector", href: "/requests" },
				{ type: "link", text: "Deliveries", href: "/deliveries" },
				{ type: "link", text: "Metrics", href: "/metrics" },
			],
		},
		{ type: "divider" },
		{
			type: "section",
			text: "Services",
			items: [...services]
				.sort((a, b) => service(a).title.localeCompare(service(b).title))
				.map((name) => ({
					type: "link",
					text: service(name).title,
					href: `/services/${name}`,
				})),
		},
	];
}

export type ConsoleLayoutProps = {
	breadcrumbs: BreadcrumbGroupProps.Item[];
	content: ReactNode;
	contentType?: AppLayoutProps.ContentType;
	splitPanel?: ReactNode;
	splitPanelOpen?: boolean;
	onSplitPanelToggle?: (open: boolean) => void;
	navigationItems?: SideNavigationProps.Item[];
	navigationHeader?: { text: string; href: string };
	activeHref?: string;
};

// ConsoleLayout is the AWS console page frame: side navigation, breadcrumbs,
// connection errors, and an optional split panel for details.
export function ConsoleLayout({
	breadcrumbs,
	content,
	contentType = "table",
	splitPanel,
	splitPanelOpen,
	onSplitPanelToggle,
	navigationItems,
	navigationHeader,
	activeHref,
}: ConsoleLayoutProps) {
	const follow = useFollow();
	const { pathname } = useLocation();
	const { data, error } = useHealth();
	const [navigationOpen, setNavigationOpen] = useState(true);
	const [splitPanelPreferences, setSplitPanelPreferences] =
		useState<AppLayoutProps.SplitPanelPreferences>({
			position: "side",
		});

	return (
		<AppLayout
			headerSelector={`#${headerID}`}
			contentType={contentType}
			toolsHide
			navigationOpen={navigationOpen}
			onNavigationChange={({ detail }) => setNavigationOpen(detail.open)}
			navigation={
				<SideNavigation
					header={navigationHeader ?? { href: "/", text: "TinyStack" }}
					activeHref={activeHref ?? pathname}
					items={navigationItems ?? navigation(data?.services ?? [])}
					onFollow={follow}
				/>
			}
			breadcrumbs={
				<BreadcrumbGroup
					items={[{ text: "TinyStack", href: "/" }, ...breadcrumbs]}
					onFollow={follow}
					ariaLabel="Breadcrumbs"
				/>
			}
			notifications={
				error && (
					<Flashbar
						items={[
							{
								type: "error",
								header: "Cannot reach TinyStack",
								content: `${error}. Start it with ./bin/tinystack start, or set TINYSTACK_URL for the dev server.`,
								id: "health",
							},
						]}
					/>
				)
			}
			content={content}
			splitPanel={splitPanel}
			splitPanelOpen={splitPanelOpen}
			onSplitPanelToggle={({ detail }) => onSplitPanelToggle?.(detail.open)}
			splitPanelPreferences={splitPanelPreferences}
			onSplitPanelPreferencesChange={({ detail }) =>
				setSplitPanelPreferences(detail)
			}
		/>
	);
}
