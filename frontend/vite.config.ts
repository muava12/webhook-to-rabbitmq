import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
	plugins: [react()],
	base: "",
	build: { outDir: process.env.OUT_DIR || "../static" },
	server: {
		proxy: {
			"/api": "http://localhost:8001",
			"/health": "http://localhost:8001",
			"/webhook": "http://localhost:8001",
		},
	},
});
