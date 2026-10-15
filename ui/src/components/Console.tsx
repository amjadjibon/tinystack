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
import { Density } from "@cloudscape-design/global-styles";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";
import { useFavorites } from "#/lib/favorites";
import { useHealth } from "#/lib/health";
import { regionLabel } from "#/lib/regions";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { consoleServiceNames, service } from "#/lib/services";
import { switchSSORole } from "#/lib/session";
import {
	applyConsoleTheme,
	applyPreferences,
	loadPreferences,
	type Preferences,
	type Theme,
	themes,
} from "#/lib/theme";
import { useFollow } from "#/lib/useFollow";
import { ConsoleSessionDialog } from "./ConsoleSession";
import { ServiceIcon } from "./ServiceIcon";

const headerID = "console-header";

// ConsoleHeader is the top bar every page shares: the TinyStack identity,
// a service search, account/Region menus, the display settings, and the
// favorites bar of starred services.
export function ConsoleHeader() {
	const navigate = useNavigate();
	const follow = useFollow();
	const { data } = useHealth();
	const [preferences, setPreferences] = useState<Preferences>(loadPreferences);
	const [query, setQuery] = useState("");
	const scope = useScope();
	const ssoAccount = scope.session?.sso ? scope.session.account : "";
	const [sessionOpen, setSessionOpen] = useState(false);
	const [sessionNotice, setSessionNotice] = useState("");
	const names = consoleServiceNames(data?.services ?? []);
	const favorites = useFavorites().filter((name) => names.includes(name));
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
					text: regionLabel(scope.region) || "Region",
					title: "AWS Region",
					ariaLabel: `AWS Region: ${scope.region || "Choose Region"}`,
					disableTextCollapse: true,
					items: [
						...scope.regions.map((region) => ({
							id: region,
							text: `${regionLabel(region)} (${region})`,
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
			text:
				ssoAccount || scope.account
					? `Account (${ssoAccount || scope.account})`
					: "Account",
			title: "AWS account",
			ariaLabel: `AWS account: ${ssoAccount || scope.account || "Choose account"}`,
			disableTextCollapse: true,
			items: ssoAccount
				? [
						...(scope.session?.sso?.roles ?? []).map((role) => ({
							id: role.role,
							text: `${role.account} — ${role.name}`,
							itemType: "checkbox" as const,
							checked: role.role === scope.session?.sso?.role,
						})),
						{
							id: "roles",
							text: "Switch role…",
							href: "/_tinystack/auth/roles",
						},
					]
				: [
						...scope.accounts.map((account) => ({
							id: account,
							text: account,
							itemType: "checkbox" as const,
							checked: account === scope.account,
						})),
						{ id: "switch", text: "Switch account…" },
					],
			onItemClick: ({ detail }) => {
				if (ssoAccount) {
					if (detail.id !== "roles") switchSSORole(detail.id);
					return;
				}
				if (detail.id === "switch") openScopeDialog("account");
				else scope.switchScope({ account: detail.id });
			},
		},
	];
	const validDraft =
		scopeDialog === "account"
			? /^\d{12}$/.test(scopeDraft)
			: validRegion(scopeDraft);

	useEffect(() => {
		const theme = applyConsoleTheme();
		return theme.reset;
	}, []);
	useEffect(() => applyPreferences(preferences), [preferences]);
	// Restore the role's account when a URL or remembered scope selects another account.
	// biome-ignore lint/correctness/useExhaustiveDependencies: switchScope changes every render.
	useEffect(() => {
		if (ssoAccount && ssoAccount !== scope.account && scope.region)
			scope.switchScope({ account: ssoAccount });
	}, [ssoAccount, scope.account, scope.region]);

	const matches = names.filter((name) =>
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
					<div className="console-search-group">
						<Button
							variant="icon"
							iconName="grid-view"
							ariaLabel="All services"
							href="/services"
							onFollow={follow}
						/>
						<ServiceSearch
							query={query}
							matches={matches}
							onChange={setQuery}
							onSelect={(name) => {
								setQuery("");
								navigate({ href: `/services/${name}` });
							}}
						/>
					</div>
				}
				utilities={[
					...(scope.capabilities.data?.requires_signature || scope.session
						? [
								{
									type: "button" as const,
									text: scope.session
										? (scope.session.sso?.name ?? "Session connected")
										: "Console session",
									iconName: "lock-private" as const,
									onClick: () => setSessionOpen(true),
								},
							]
						: []),
					{
						type: "button",
						iconName: "command-prompt",
						ariaLabel: "CloudShell",
						title: "CloudShell",
						href: "/cloudshell",
						onFollow: follow,
					},
					{
						type: "button",
						iconName: "upload-download",
						ariaLabel: "Request inspector",
						title: "Request inspector",
						href: "/requests",
						onFollow: follow,
					},
					{
						type: "menu-dropdown",
						iconName: "settings",
						ariaLabel: "Display settings",
						title: "Display settings",
						items: [
							{
								id: "theme",
								text: "Theme",
								items: themes.map(({ id, text }) => ({
									id,
									text,
									checked: preferences.theme === id,
									itemType: "checkbox" as const,
								})),
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
							if (themes.some(({ id }) => id === detail.id)) {
								setPreferences((p) => ({ ...p, theme: detail.id as Theme }));
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
			{favorites.length > 0 && (
				<nav className="console-favorites" aria-label="Favorites">
					{favorites.map((name) => (
						<Link key={name} to="/services/$service" params={{ service: name }}>
							<ServiceIcon name={name} size="small" />
							{service(name).title}
						</Link>
					))}
				</nav>
			)}
			{sessionOpen ? (
				<ConsoleSessionDialog
					notice={sessionNotice}
					onDismiss={() => {
						setSessionOpen(false);
						setSessionNotice("");
					}}
				/>
			) : null}
			{scopeDialog && !(scopeDialog === "account" && ssoAccount) && (
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
	services = consoleServiceNames(services);
	return [
		{ type: "link", text: "Console home", href: "/" },
		{ type: "link", text: "All services", href: "/services" },
		{ type: "link", text: "All resources", href: "/resources" },
		{ type: "link", text: "CloudShell", href: "/cloudshell" },
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
			ariaLabels={{
				navigation: "Side navigation",
				navigationToggle: "Open side navigation",
				navigationClose: "Close side navigation",
			}}
			maxContentWidth={Number.MAX_VALUE}
			toolsHide
			navigationOpen={navigationOpen}
			onNavigationChange={({ detail }) => setNavigationOpen(detail.open)}
			navigation={
				<SideNavigation
					header={navigationHeader ?? { href: "/", text: "Console home" }}
					activeHref={activeHref ?? pathname}
					items={
						navigationItems ??
						navigation(
							contentType === "dashboard" ? [] : (data?.services ?? []),
						)
					}
					onFollow={follow}
				/>
			}
			breadcrumbs={
				contentType !== "dashboard" && (
					<BreadcrumbGroup
						items={[{ text: "TinyStack", href: "/" }, ...breadcrumbs]}
						onFollow={follow}
						ariaLabel="Breadcrumbs"
					/>
				)
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
