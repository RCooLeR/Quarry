import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Vitest 5 reuses jsdom within VM workers while retaining file isolation.
    // Limit parallel environments on high-core hosts to avoid memory spikes.
    pool: "vmThreads",
    maxWorkers: 4,
    environment: "jsdom",
    environmentOptions: {
      jsdom: {
        pretendToBeVisual: true,
      },
    },
    include: ["tests/**/*.test.ts"],
    setupFiles: ["./tests/setup.ts"],
  },
});
