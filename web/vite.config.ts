import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// Single-binary deploy: Go embeds ./dist, so keep asset paths relative.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "/",
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  // emptyOutDir false: never delete web/dist/.gitkeep (fresh clones need dist/
  // to exist for //go:embed to compile before the first `bun run build`).
  build: { outDir: "dist", emptyOutDir: false },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/auth": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
      "/webhooks": "http://localhost:8080",
    },
  },
});
