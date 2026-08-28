import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The build lands in ../static, which the Go transport embeds; it is
// committed so `go build` needs no Node. `npm run dev` proxies the API to
// a running `dispatch run -web`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "../static", emptyOutDir: true, sourcemap: false },
  server: { proxy: { "/api": { target: "http://127.0.0.1:8788", changeOrigin: true } } },
});
