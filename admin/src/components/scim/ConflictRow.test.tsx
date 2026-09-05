import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { ConflictRow } from "./ConflictRow";
import {
  asConflictError,
  conflictGuidance,
  type ConflictErrorCode,
} from "@/lib/conflictError";

// ConflictRow calls useMutation at the top level. Mock it so the component mounts
// without an ApolloProvider; the display/state tests never invoke a submit.
vi.mock("@apollo/client/react", () => ({
  useMutation: () => [vi.fn(), false],
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
// ErrorPresenter (FORBIDDEN / NOT_FOUND / CONFLICT / …). asConflictError reads
// err.graphQLErrors[].extensions.code for the code AND err.message (top-level,
// as a real ApolloError carries) for the message.
function codedError(code: ConflictErrorCode, message: string) {
  return {
    message,
    graphQLErrors: [{ message, extensions: { code, status: 403 } }],
  };
}

describe("asConflictError", () => {
  it("maps a recognized extensions.code verbatim", () => {
    expect(asConflictError(codedError("FORBIDDEN", "no")).code).toBe("FORBIDDEN");
    expect(asConflictError(codedError("NOT_FOUND", "no")).code).toBe("NOT_FOUND");
    expect(asConflictError(codedError("CONFLICT", "no")).code).toBe("CONFLICT");
  });

  it("collapses a missing/absent code to INTERNAL — never a denial", () => {
    // apperr.UserError surfaces verbatim with NO code → must be INTERNAL.
    const noCode = { graphQLErrors: [{ message: "refused" }] };
    expect(asConflictError(noCode).code).toBe("INTERNAL");

    // Unknown code → INTERNAL (whitelist, not a range).
    const weird = {
      graphQLErrors: [{ message: "x", extensions: { code: "TEAPOT" } }],
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
});
