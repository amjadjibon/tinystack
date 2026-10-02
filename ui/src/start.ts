import { createStart } from "@tanstack/react-start";

// The console is a single-page app: Cloudscape renders in the browser, which
// is where the data it shows comes from.
export const startInstance = createStart(() => ({ defaultSsr: false }));
