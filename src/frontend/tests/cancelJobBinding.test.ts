import { beforeEach, describe, expect, it, vi } from "vitest";

const runtime = vi.hoisted(() => ({
  byID: vi.fn(() => Promise.resolve()),
}));

vi.mock("@wailsio/runtime", () => ({
  Call: { ByID: runtime.byID },
  CancellablePromise: class {},
  Create: {
    Any: (value: unknown) => value,
    Array: (create: (value: unknown) => unknown) => (value: unknown[]) => value.map(create),
    Nullable: (create: (value: unknown) => unknown) => (value: unknown) => value == null ? null : create(value),
  },
}));

import { CancelJob } from "../bindings/github.com/quarry/quarry-wails3/fileservice.js";

describe("generated CancelJob binding", () => {
  beforeEach(() => runtime.byID.mockClear());

  it("sends the expected job ID across the generated bridge", async () => {
    await CancelJob("job42");
    expect(runtime.byID).toHaveBeenCalledOnce();
    expect(runtime.byID).toHaveBeenCalledWith(2661738260, "job42");
  });
});
