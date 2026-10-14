// AWS Node runtimes also resolve ESM imports from the layer NODE_PATH.
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
const require = createRequire(import.meta.url);
// fallback names the NODE_PATH module a bare specifier resolves to, or rethrows the original error.
export function fallback(specifier, error) {
  if (error.code !== 'ERR_MODULE_NOT_FOUND' || specifier.startsWith('.') || specifier.startsWith('/') ||
      specifier.includes(':')) throw error;
  try {
    return pathToFileURL(require.resolve(specifier)).href;
  } catch {
    throw error;
  }
}
export async function resolve(specifier, context, nextResolve) {
  try {
    return await nextResolve(specifier, context);
  } catch (error) {
    return nextResolve(fallback(specifier, error), context);
  }
}
