import { devtools } from "@tanstack/devtools-vite";
import { tanstackStart } from "@tanstack/react-start/plugin/vite";
import viteReact from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The console reads TinyStack's /_tinystack/ endpoints; in development they
// are proxied to the server at TINYSTACK_URL.
const tinystack = process.env.TINYSTACK_URL ?? "http://127.0.0.1:4566";

const config = defineConfig({
	resolve: { tsconfigPaths: true },
	server: { proxy: { "/_tinystack": tinystack } },
	ssr: { noExternal: [/^@cloudscape-design\//] },
	plugins: [devtools(), tanstackStart({ spa: { enabled: true } }), viteReact()],
});

export default config;
