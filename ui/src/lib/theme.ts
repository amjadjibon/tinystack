import { applyTheme } from "@cloudscape-design/components/theming";
import {
	applyDensity,
	applyMode,
	Density,
	Mode,
} from "@cloudscape-design/global-styles";

export type Preferences = { mode: Mode; density: Density };

const key = "tinystack.console.preferences";

export function loadPreferences(): Preferences {
	try {
		const saved = JSON.parse(localStorage.getItem(key) ?? "{}");
		return {
			mode:
				saved.version === 2 && saved.mode === Mode.Light
					? Mode.Light
					: Mode.Dark,
			density:
				saved.density === Density.Compact
					? Density.Compact
					: Density.Comfortable,
		};
	} catch {
		return { mode: Mode.Dark, density: Density.Comfortable };
	}
}

export function applyPreferences(preferences: Preferences) {
	applyMode(preferences.mode);
	applyDensity(preferences.density);
	try {
		localStorage.setItem(key, JSON.stringify({ ...preferences, version: 2 }));
	} catch {
		// Display settings still apply when browser storage is unavailable.
	}
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
