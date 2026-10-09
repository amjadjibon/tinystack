// Gzips the console build (dist/client) into dist/console, the tree the
// console Go build tag embeds and serves compressed.
import { mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join, relative } from "node:path";
import { constants, gzipSync } from "node:zlib";

const from = "dist/client";
const to = "dist/console";

await rm(to, { recursive: true, force: true });
for (const entry of await readdir(from, {
	recursive: true,
	withFileTypes: true,
})) {
	if (!entry.isFile()) {
		continue;
	}

	const source = join(entry.parentPath, entry.name);
	const target = `${join(to, relative(from, source))}.gz`;
	await mkdir(dirname(target), { recursive: true });
	await writeFile(
		target,
		gzipSync(await readFile(source), { level: constants.Z_BEST_COMPRESSION }),
	);
}
