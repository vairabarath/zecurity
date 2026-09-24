import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { CombinedGraphQLErrors } from "@apollo/client/errors";
import { ConflictRow } from "./ConflictRow";
import {
  asConflictError,
  conflictGuidance,
  type ConflictErrorCode,
} from "@/lib/conflictError";

// ConflictRow (and the GrantBreakGlassButton it renders on the FORBIDDEN path)
// each call useMutation at the top level, keyed on distinct documents. The
// display/state tests need divergent per-mutation behaviour, so dispatch on the
// document's operation name rather than returning one shared tuple.
//
// Per-mutation runtime behaviour is swapped by mutating these handlers. Each
// handler is the async function Apollo's `mutate` returns; a rejection models a
// server error carrying extensions.code.
const handlers: Record<string, ReturnType<typeof vi.fn>> = {
  AcceptScimConflict: vi.fn().mockResolvedValue({ data: {} }),
  RejectScimConflict: vi.fn().mockResolvedValue({ data: {} }),
  ReopenScimConflict: vi.fn().mockResolvedValue({ data: {} }),
  GrantWorkspacePermission: vi.fn().mockResolvedValue({ data: {} }),
};

function opName(doc: unknown): string {
  const defs = (doc as { definitions?: Array<{ name?: { value?: string } }> })?.definitions;
  return defs?.[0]?.name?.value ?? "";
}

vi.mock("@apollo/client/react", () => ({
  // Second tuple element is a bare `false` (matching the original mock), so
  // result.loading reads as undefined and `busy` stays falsy everywhere —
  // enabled-button assertions hold.
  useMutation: (doc: unknown) => [handlers[opName(doc)] ?? vi.fn(), false],
}));

// GrantBreakGlassButton reads the current user from the auth store.
vi.mock("@/store/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } | null }) => unknown) =>
    selector({ user: { id: "admin-1" } }),
}));

// Minimal ScimConflict factory. Status drives the actions available.
function conflict(
  over: Partial<{
    status: string;
    scimUsernameSnapshot: string | null;
    scimEmailSnapshot: string | null;
    canonicalKey: string;
    resolutionReason: string | null;
  }> = {},
) {
  return {
    __typename: "ScimConflict" as const,
    id: "c1",
    workspaceId: "w1",
    connectionId: "conn1",
    userId: "u1",
    canonicalKey: over.canonicalKey ?? "sub:abc123",
    scimExternalId: "ext-1",
    scimUsernameSnapshot: over.scimUsernameSnapshot ?? null,
    scimEmailSnapshot: over.scimEmailSnapshot ?? null,
    status: over.status ?? "pending",
    resolutionReason: over.resolutionReason ?? null,
    createdAt: "2026-08-26T00:00:00Z",
    resolvedAt: null,
  };
}

// Build an error carrying a structured extensions.code, mirroring the backend
// ErrorPresenter (FORBIDDEN / NOT_FOUND / CONFLICT / …).
//
// Constructed from the REAL Apollo Client 4 class, not a hand-rolled shape. The
// previous fixture fabricated a v3-style `graphQLErrors` array, so every test
// here validated the mock's contract instead of Apollo's — and the shipped code
// read a field that no longer exists, collapsing FORBIDDEN to INTERNAL in the
// real app while the suite stayed green. Pinning the fixture to the library is
// what makes these assertions mean anything.
//
// vi.mock() below only shadows "@apollo/client/react", so this import is live.
function codedError(code: ConflictErrorCode, message: string) {
  return new CombinedGraphQLErrors({
    errors: [{ message, extensions: { code, status: 403 } }],
  });
}

const GRANT_LABEL = "Grant myself identity.mapping.break_glass";

describe("asConflictError", () => {
  it("maps a recognized extensions.code verbatim", () => {
    expect(asConflictError(codedError("FORBIDDEN", "no")).code).toBe("FORBIDDEN");
    expect(asConflictError(codedError("NOT_FOUND", "no")).code).toBe("NOT_FOUND");
    expect(asConflictError(codedError("CONFLICT", "no")).code).toBe("CONFLICT");
  });

  it("collapses a missing/absent code to INTERNAL — never a denial", () => {
    // apperr.UserError surfaces verbatim with NO code → must be INTERNAL.
    const noCode = new CombinedGraphQLErrors({
      errors: [{ message: "refused" }],
    });
    expect(asConflictError(noCode).code).toBe("INTERNAL");

    // Unknown code → INTERNAL (whitelist, not a range).
    const weird = {
      errors: [{ message: "x", extensions: { code: "TEAPOT" } }],
    };
    expect(asConflictError(weird).code).toBe("INTERNAL");
  });

  it("preserves the server message", () => {
    expect(asConflictError(codedError("FORBIDDEN", "needs break_glass")).message).toBe(
      "needs break_glass",
    );
  });
});

describe("conflictGuidance", () => {
  it("FORBIDDEN points at the break_glass permission", () => {
    const g = conflictGuidance({
      code: "FORBIDDEN",
      message: "requires identity.mapping.break_glass",
    });
    expect(g.title).toBe("Permission required");
    expect(g.body).toContain("identity.mapping.break_glass");
  });

  it("NOT_FOUND / CONFLICT tells the admin to refresh", () => {
    const g = conflictGuidance({ code: "NOT_FOUND", message: "gone" });
    expect(g.title).toBe("Conflict changed");
    expect(g.body).toContain("Refresh the queue");
  });

  it("INTERNAL is a generic failure, never a permission hint", () => {
    const g = conflictGuidance({ code: "INTERNAL", message: "boom" });
    expect(g.title).toBe("Action failed");
    expect(g.body).toBe("boom");
  });
});

describe("ConflictRow", () => {
  const onResolved = vi.fn();

  beforeEach(() => {
    onResolved.mockReset();
    handlers.AcceptScimConflict.mockReset().mockResolvedValue({ data: {} });
    handlers.RejectScimConflict.mockReset().mockResolvedValue({ data: {} });
    handlers.ReopenScimConflict.mockReset().mockResolvedValue({ data: {} });
    handlers.GrantWorkspacePermission.mockReset().mockResolvedValue({ data: {} });
  });

  it("shows Accept and Reject for a pending row; shows no reason", () => {
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    expect(screen.getByText("Accept link")).toBeInTheDocument();
    expect(screen.getByText("Reject")).toBeInTheDocument();
    expect(screen.queryByText(/reason:/)).not.toBeInTheDocument();
  });

  it("falls back to canonicalKey when both snapshots are null", () => {
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    // The sub-line renders "canonical key: <key> · external id: <id>"; assert the
    // key appears within the paragraph element (match the <p> to avoid ancestor hits).
    expect(
      screen.getByText(
        (_, el) =>
          el?.nodeName === "P" &&
          (el?.textContent?.includes("canonical key: sub:abc123") ?? false),
      ),
    ).toBeInTheDocument();
  });

  it("prefers the directory snapshot as the human-readable context", () => {
    render(
      <ConflictRow
        conflict={conflict({ scimUsernameSnapshot: "alice@okta" })}
        connectionId="conn1"
        onResolved={onResolved}
      />,
    );
    expect(screen.getByText("alice@okta")).toBeInTheDocument();
  });

  it("shows the resolution reason on a resolved (approved) row", () => {
    render(
      <ConflictRow
        conflict={conflict({ status: "approved", resolutionReason: "linked to existing" })}
        connectionId="conn1"
        onResolved={onResolved}
      />,
    );
    expect(screen.getByText("reason: linked to existing")).toBeInTheDocument();
    // No actions on a resolved row.
    expect(screen.queryByText("Accept link")).not.toBeInTheDocument();
  });

  it("shows Reopen (only) for a rejected row", () => {
    render(
      <ConflictRow
        conflict={conflict({ status: "rejected", resolutionReason: "wrong user" })}
        connectionId="conn1"
        onResolved={onResolved}
      />,
    );
    expect(screen.getByText("Reopen")).toBeInTheDocument();
    expect(screen.queryByText("Accept link")).not.toBeInTheDocument();
    expect(screen.queryByText("Reject")).not.toBeInTheDocument();
  });

  it("renders expired defensively and read-only (no actions)", () => {
    render(
      <ConflictRow
        conflict={conflict({ status: "expired", resolutionReason: "stale" })}
        connectionId="conn1"
        onResolved={onResolved}
      />,
    );
    expect(screen.queryByText("Accept link")).not.toBeInTheDocument();
    expect(screen.queryByText("Reject")).not.toBeInTheDocument();
    expect(screen.queryByText("Reopen")).not.toBeInTheDocument();
  });

  it("requires a reason before Confirm is enabled", async () => {
    const user: UserEvent = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await user.click(screen.getByText("Reject"));
    const confirm = screen.getByRole("button", { name: "Reject" });
    expect(confirm).toBeDisabled();
    await user.type(screen.getByLabelText("Reason"), "duplicate identity");
    await waitFor(() => expect(confirm).toBeEnabled());
  });

  // ─── PENDING-05 §10 coverage: FORBIDDEN-path grant affordance ───

  // Opens the accept dialog, types a reason, and confirms once — driving the
  // accept mutation to whatever handler is currently configured.
  async function openAcceptAndConfirm(user: UserEvent, reason = "linking duplicate") {
    await user.click(screen.getByText("Accept link"));
    await user.type(screen.getByLabelText("Reason"), reason);
    const confirm = screen.getByRole("button", { name: "Accept link" });
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);
  }

  it("FORBIDDEN on accept → guidance AND grant action rendered; dialog stays open", async () => {
    handlers.AcceptScimConflict.mockRejectedValue(
      codedError("FORBIDDEN", "requires identity.mapping.break_glass"),
    );
    const user = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await openAcceptAndConfirm(user);

    await waitFor(() => expect(screen.getByText("Permission required")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: GRANT_LABEL })).toBeInTheDocument();
    // Dialog stays open (reason field still present); accept was not resolved.
    expect(screen.getByLabelText("Reason")).toBeInTheDocument();
    expect(onResolved).not.toHaveBeenCalled();
  });

  it.each(["INTERNAL", "CONFLICT", "NOT_FOUND"] as const)(
    "%s on accept → no grant action",
    async (code) => {
      handlers.AcceptScimConflict.mockRejectedValue(codedError(code, "nope"));
      const user = userEvent.setup();
      render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
      await openAcceptAndConfirm(user);

      // Dialog is still open (guidance shown) but no grant affordance appears.
      await waitFor(() => expect(screen.getByLabelText("Reason")).toBeInTheDocument());
      expect(screen.queryByRole("button", { name: GRANT_LABEL })).not.toBeInTheDocument();
    },
  );

  it("grant succeeds → accept is NOT auto-retried; a second explicit Confirm fires it", async () => {
    handlers.AcceptScimConflict.mockRejectedValue(
      codedError("FORBIDDEN", "requires identity.mapping.break_glass"),
    );
    const user = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await openAcceptAndConfirm(user);

    const grant = await screen.findByRole("button", { name: GRANT_LABEL });
    expect(handlers.AcceptScimConflict).toHaveBeenCalledTimes(1);

    await user.click(grant);
    await waitFor(() => expect(handlers.GrantWorkspacePermission).toHaveBeenCalledTimes(1));
    // Grant does NOT re-fire the accept.
    expect(handlers.AcceptScimConflict).toHaveBeenCalledTimes(1);

    // A second explicit Confirm is required to retry the accept.
    await user.click(screen.getByRole("button", { name: "Accept link" }));
    await waitFor(() => expect(handlers.AcceptScimConflict).toHaveBeenCalledTimes(2));
  });

  it("grant fails → error surfaced, accept still blocked, dialog still open", async () => {
    handlers.AcceptScimConflict.mockRejectedValue(
      codedError("FORBIDDEN", "requires identity.mapping.break_glass"),
    );
    handlers.GrantWorkspacePermission.mockRejectedValue({ message: "grant refused" });
    const user = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await openAcceptAndConfirm(user);

    const grant = await screen.findByRole("button", { name: GRANT_LABEL });
    await user.click(grant);

    await waitFor(() => expect(handlers.GrantWorkspacePermission).toHaveBeenCalledTimes(1));
    // Dialog still open; accept never advanced.
    expect(screen.getByLabelText("Reason")).toBeInTheDocument();
    expect(handlers.AcceptScimConflict).toHaveBeenCalledTimes(1);
    expect(onResolved).not.toHaveBeenCalled();
  });

  it("reason persists across the grant step", async () => {
    handlers.AcceptScimConflict.mockRejectedValue(
      codedError("FORBIDDEN", "requires identity.mapping.break_glass"),
    );
    const user = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await openAcceptAndConfirm(user, "keep this reason");

    const grant = await screen.findByRole("button", { name: GRANT_LABEL });
    await user.click(grant);
    await waitFor(() => expect(handlers.GrantWorkspacePermission).toHaveBeenCalledTimes(1));

    // The typed reason survives the grant (dialog not remounted, field not cleared).
    expect(screen.getByLabelText("Reason")).toHaveValue("keep this reason");
  });

  it("Reject never renders the grant action even on FORBIDDEN (dialog === 'accept' half)", async () => {
    // Reject cannot 403 in practice, but force a FORBIDDEN to prove the
    // dialog === 'accept' half of the guard suppresses the grant affordance.
    handlers.RejectScimConflict.mockRejectedValue(codedError("FORBIDDEN", "nope"));
    const user = userEvent.setup();
    render(<ConflictRow conflict={conflict()} connectionId="conn1" onResolved={onResolved} />);
    await user.click(screen.getByText("Reject"));
    await user.type(screen.getByLabelText("Reason"), "rejecting");
    const confirm = screen.getByRole("button", { name: "Reject" });
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);

    // Error surfaced but no grant affordance — it is gated to the accept dialog.
    await waitFor(() => expect(handlers.RejectScimConflict).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("button", { name: GRANT_LABEL })).not.toBeInTheDocument();
  });

  it("Reopen never renders the grant action", async () => {
    handlers.ReopenScimConflict.mockRejectedValue(codedError("FORBIDDEN", "nope"));
    const user = userEvent.setup();
    render(
      <ConflictRow
        conflict={conflict({ status: "rejected", resolutionReason: "wrong user" })}
        connectionId="conn1"
        onResolved={onResolved}
      />,
    );
    await user.click(screen.getByText("Reopen"));
    await user.type(screen.getByLabelText("Reason"), "reopening");
    const confirm = screen.getByRole("button", { name: "Reopen" });
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);

    await waitFor(() => expect(handlers.ReopenScimConflict).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("button", { name: GRANT_LABEL })).not.toBeInTheDocument();
  });
});
