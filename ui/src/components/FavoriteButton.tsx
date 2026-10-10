import { ToggleButton } from "@cloudscape-design/components";
import { toggleFavorite, useFavorites } from "#/lib/favorites";
import { service } from "#/lib/services";

// FavoriteButton stars a service, which puts it on the top bar's favorites.
export function FavoriteButton({ name }: { name: string }) {
	return (
		<ToggleButton
			variant="icon"
			iconName="star"
			pressedIconName="star-filled"
			pressed={useFavorites().includes(name)}
			ariaLabel={`Favorite ${service(name).title}`}
			onChange={() => toggleFavorite(name)}
		/>
	);
}
