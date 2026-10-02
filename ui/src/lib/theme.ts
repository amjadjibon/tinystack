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
			mode: saved.mode === Mode.Dark ? Mode.Dark : Mode.Light,
			density:
				saved.density === Density.Compact
					? Density.Compact
					: Density.Comfortable,
		};
	} catch {
		return { mode: Mode.Light, density: Density.Comfortable };
	}
}

export function applyPreferences(preferences: Preferences) {
	applyMode(preferences.mode);
	applyDensity(preferences.density);
	localStorage.setItem(key, JSON.stringify(preferences));
}
