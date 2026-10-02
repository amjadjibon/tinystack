import { useNavigate } from "@tanstack/react-router";
import { useCallback } from "react";

type FollowDetail = { href?: string; external?: boolean };

// useFollow returns an onFollow handler that sends Cloudscape links, menus,
// and breadcrumbs through the router instead of reloading the page.
export function useFollow() {
	const navigate = useNavigate();
	return useCallback(
		(event: CustomEvent<FollowDetail>) => {
			const { href, external } = event.detail;
			if (!href || external || href.startsWith("http")) return;

			event.preventDefault();
			navigate({ href });
		},
		[navigate],
	);
}
