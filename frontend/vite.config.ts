import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const outDir = "../cmd/pigo-server/web/dist";

export default defineConfig({
  plugins: [
    react(),
    // The build output is not in git; only dist/.gitkeep is, so the server's
    // //go:embed has a directory to embed in a fresh checkout. emptyOutDir
    // clears it with the rest, so every build emits it again.
    {
      name: "keep-dist-placeholder",
      generateBundle() {
        this.emitFile({ type: "asset", fileName: ".gitkeep", source: "" });
      },
    },
  ],
  build: {
    emptyOutDir: true,
    outDir,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/healthz": "http://127.0.0.1:8080",
    },
  },
});
