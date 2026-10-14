// Installs the NODE_PATH fallback in loader.mjs: synchronous hooks where Node has them (no warnings), the
// loader thread otherwise.
import module from 'node:module';
import { fallback } from './loader.mjs';
if (module.registerHooks) {
  module.registerHooks({
    resolve(specifier, context, nextResolve) {
      try {
        return nextResolve(specifier, context);
      } catch (error) {
        return nextResolve(fallback(specifier, error), context);
      }
    }
  });
} else {
  module.register(new URL('./loader.mjs', import.meta.url));
}
