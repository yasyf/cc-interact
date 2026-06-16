// Side-effect CSS imports exist only to pull base.css into the build graph so
// Vite emits dist/base.css; they carry no runtime value.
declare module '*.css';
