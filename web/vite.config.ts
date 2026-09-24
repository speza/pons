import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // The Go server embeds web/dist. Only the app subdirectory is generated,
  // so the committed web/dist/.gitkeep keeps the embed valid without a build.
  build: {
    outDir: "dist/app",
  },
  server: {
    proxy: {
      "/v1": "http://127.0.0.1:7337",
      "/healthz": "http://127.0.0.1:7337",
    },
  },
});
