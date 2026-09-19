import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { GrantBreakGlassButton } from "./GrantBreakGlassButton";

// The grant mutation function, swapped per test. Default resolves successfully.
const grantMock = vi.fn().mockResolvedValue({ data: {} });

vi.mock("@apollo/client/react", () => ({
  useMutation: () => [grantMock, { loading: false }],
}));

// Current user is swapped per test to exercise the no-authenticated-user guard.
let currentUser: { id: string } | null = { id: "user-1" };
vi.mock("@/store/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } | null }) => unknown) =>
    selector({ user: currentUser }),
}));

const toast = { success: vi.fn(), error: vi.fn() };
vi.mock("sonner", () => ({ toast: { success: (m: string) => toast.success(m), error: (m: string) => toast.error(m) } }));

describe("GrantBreakGlassButton", () => {
  beforeEach(() => {
    grantMock.mockReset().mockResolvedValue({ data: {} });
    toast.success.mockReset();
    toast.error.mockReset();
    currentUser = { id: "user-1" };
  });

  it("grants the break-glass permission, fires onGranted, and disables on success", async () => {
    const user = userEvent.setup();
    const onGranted = vi.fn();
    render(<GrantBreakGlassButton successMessage="granted!" onGranted={onGranted} />);

    const btn = screen.getByRole("button", { name: "Grant myself identity.mapping.break_glass" });
    await user.click(btn);

    await waitFor(() =>
      expect(grantMock).toHaveBeenCalledWith({
        variables: { userId: "user-1", permission: "identity.mapping.break_glass" },
      }),
    );
    expect(onGranted).toHaveBeenCalledTimes(1);
    expect(toast.success).toHaveBeenCalledWith("granted!");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Break-glass permission granted" })).toBeDisabled(),
    );
  });

  it("surfaces the server error and does not fire onGranted on failure", async () => {
    grantMock.mockRejectedValueOnce({ message: "server said no" });
    const user = userEvent.setup();
    const onGranted = vi.fn();
    render(<GrantBreakGlassButton successMessage="granted!" onGranted={onGranted} />);

    await user.click(
      screen.getByRole("button", { name: "Grant myself identity.mapping.break_glass" }),
    );

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("server said no"));
    expect(onGranted).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
    // Button is not marked granted; label remains actionable.
    expect(
      screen.getByRole("button", { name: "Grant myself identity.mapping.break_glass" }),
    ).toBeEnabled();
  });

  it("refuses when there is no authenticated user (no mutation, no onGranted)", async () => {
    currentUser = null;
    const user = userEvent.setup();
    const onGranted = vi.fn();
    render(<GrantBreakGlassButton successMessage="granted!" onGranted={onGranted} />);

    await user.click(
      screen.getByRole("button", { name: "Grant myself identity.mapping.break_glass" }),
    );

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("No authenticated user — cannot grant permission."),
    );
    expect(grantMock).not.toHaveBeenCalled();
    expect(onGranted).not.toHaveBeenCalled();
  });
});
