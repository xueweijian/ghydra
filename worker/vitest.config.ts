import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    // 纯逻辑测试（依赖全注入）：node 环境即可，workers 运行时行为由真网实测兜底
    environment: "node",
  },
});
