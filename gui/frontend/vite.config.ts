import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// base './'：dist 被 Wails 壳（file/wails://）与 ghydra serve 托管
// （syncthing 兜底模式）共用，必须用相对路径。
export default defineConfig({
  plugins: [solid()],
  base: "./",
  build: {
    outDir: "dist",
    target: "chrome110",
  },
  server: {
    proxy: {
      // 开发期直连本机 daemon
      "/status": "http://127.0.0.1:9801",
      "/pac": "http://127.0.0.1:9801",
      "/api": "http://127.0.0.1:9801",
    },
  },
});
