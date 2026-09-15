import fs from "node:fs";
import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// base './'：dist 被 Wails 壳（file/wails://）与 ghydra serve 托管
// （syncthing 兜底模式）共用，必须用相对路径。
export default defineConfig({
  plugins: [
    solid(),
    {
      // emptyOutDir 会连 dist/.gitkeep 一起清掉——它是 go:embed 的
      // 编译 stub（CI 非 frontend 作业的 gui 构建靠它过编译），构建后
      // 自动回写，防本地 `npm run build` + `git add -A` 再误删（F11
      // 基础设施，2026-09-15 nsis-path 失败实证）。
      name: "restore-dist-gitkeep",
      closeBundle() {
        fs.writeFileSync("dist/.gitkeep", "");
      },
    },
  ],
  base: "./",
  build: {
    outDir: "dist",
    target: "chrome110",
  },
  test: {
    environment: "node", // golden 契约测试无需 DOM
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
