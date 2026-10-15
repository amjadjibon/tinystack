import { applyTheme } from "@cloudscape-design/components/theming";
import {
	applyDensity,
	applyMode,
	Density,
	Mode,
} from "@cloudscape-design/global-styles";

// Theme is a person's choice of colors: light, dark, or whichever the
// operating system uses, following it when it changes.
export type Theme = Mode | "system";
export type Preferences = { theme: Theme; density: Density };

export const themes: { id: Theme; text: string }[] = [
	{ id: "system", text: "System" },
	{ id: Mode.Light, text: "Light" },
	{ id: Mode.Dark, text: "Dark" },
];

const key = "tinystack.console.preferences";

export function loadPreferences(): Preferences {
	try {
		const saved = JSON.parse(localStorage.getItem(key) ?? "{}");
		return {
			theme: themes.some(({ id }) => id === saved.theme)
				? saved.theme
				: "system",
			density:
				saved.density === Density.Compact
					? Density.Compact
					: Density.Comfortable,
		};
	} catch {
		return { theme: "system", density: Density.Comfortable };
	}
}

// applyPreferences applies and saves preferences. It returns a function that
// stops following the operating system's theme, for a React effect to clean up.
export function applyPreferences(preferences: Preferences) {
	applyDensity(preferences.density);
	try {
		localStorage.setItem(key, JSON.stringify(preferences));
	} catch {
		// Display settings still apply when browser storage is unavailable.
	}

	if (preferences.theme !== "system") {
		applyMode(preferences.theme);
		return () => {};
	}

	const dark = window.matchMedia("(prefers-color-scheme: dark)");
	const follow = () => applyMode(dark.matches ? Mode.Dark : Mode.Light);
	follow();
	dark.addEventListener("change", follow);
	return () => dark.removeEventListener("change", follow);
}

// Public Cloudscape theme tokens keep every service view consistent with the shell.
export function applyConsoleTheme() {
	return applyTheme({
		theme: {
			tokens: {
				colorBackgroundLayoutMain: { light: "#f2f3f3", dark: "#10171e" },
				colorBackgroundContainerContent: { light: "#ffffff", dark: "#161e27" },
				colorBackgroundContainerHeader: { light: "#ffffff", dark: "#161e27" },
				colorBackgroundLayoutPanelContent: {
					light: "#ffffff",
					dark: "#161e27",
				},
				colorBackgroundLayoutPanel: { light: "#ffffff", dark: "#161e27" },
				colorBackgroundLayoutToolbar: { light: "#ffffff", dark: "#161e27" },
				colorBackgroundButtonPrimaryDefault: "#ff9900",
				colorBackgroundButtonPrimaryHover: "#ec8b00",
				colorBackgroundButtonPrimaryActive: "#df8400",
				colorBorderButtonPrimaryDefault: "#ff9900",
				colorBorderButtonPrimaryHover: "#ec8b00",
				colorBorderButtonPrimaryActive: "#df8400",
				colorTextButtonPrimaryDefault: "#10171e",
				colorTextButtonPrimaryHover: "#10171e",
				colorTextButtonPrimaryActive: "#10171e",
				colorTextLinkDefault: { light: "#006ce0", dark: "#3bb4ff" },
				colorTextLinkHover: { light: "#003e87", dark: "#79cbff" },
				colorBorderButtonNormalDefault: { light: "#006ce0", dark: "#3bb4ff" },
				colorTextButtonNormalDefault: { light: "#006ce0", dark: "#3bb4ff" },
				borderRadiusContainer: "16px",
			},
		},
	});
}
