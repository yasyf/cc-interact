import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// Library mode: react / react-dom / @tanstack/react-query stay external (peerDependencies)
// so a consumer's single React copy is the only one — a second copy breaks hooks.
export default defineConfig({
  plugins: [react()],
  build: {
    lib: {
      entry: fileURLToPath(new URL('src/index.ts', import.meta.url)),
      formats: ['es', 'cjs'],
      fileName: (format) => (format === 'es' ? 'index.js' : 'index.cjs'),
      // Emit the bundled chrome as dist/base.css; consumers import it explicitly
      // via the package's ./base.css export.
      cssFileName: 'base',
    },
    rollupOptions: {
      external: ['react', 'react-dom', 'react/jsx-runtime', '@tanstack/react-query'],
    },
  },
});
