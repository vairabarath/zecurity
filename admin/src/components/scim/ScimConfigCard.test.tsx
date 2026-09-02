import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ScimConfigCard, type ScimConfigConnection } from "./ScimConfigCard";

// ScimConfigCard calls useQuery (provider profiles) + useMutation at the top
// level, and renders the BreakGlassDialog child. Mock Apollo so the card mounts
// without an ApolloProvider, and mock the dialog (it owns its own mutation) so
// we only assert the card's own always-rendered controls.
vi.mock("@apollo/client/react", () => ({
  useQuery: () => ({ data: undefined, loading: false, error: undefined }),
  useMutation: () => [vi.fn(), { loading: false }],
}));
vi.mock("./BreakGlassDialog", () => ({
  BreakGlassDialog: () => null,
}));

function baseConnection(over: Partial<ScimConfigConnection> = {}): ScimConfigConnection {
  return {
    id: "conn-1",
    displayName: "Okta",
    provider: "okta",
    managed: false,
    subjectClaim: "sub",
    scimIdentifier: "externalId",
    scimEnabled: false,
    ...over,
  };
}

// F7-4: a render test that FAILS if the enable Switch is absent from the DOM.
// This is the exact manual-gate miss from Phase 1 — the "Toggle SCIM
// provisioning" Switch and the "SCIM configuration" header must always mount,
// regardless of enabled/disabled state, so an admin can never be locked out of
// the break-glass enable flow from the UI.
describe("ScimConfigCard — enable controls always render (F7-4)", () => {
  it("renders the SCIM configuration header", () => {
    render(<ScimConfigCard connection={baseConnection()} onChanged={() => {}} />);
    expect(screen.getByText("SCIM configuration")).toBeInTheDocument();
  });

  it("renders the Toggle SCIM provisioning switch (role=switch) when disabled", () => {
    render(<ScimConfigCard connection={baseConnection({ scimEnabled: false })} onChanged={() => {}} />);
    const toggle = screen.getByRole("switch", { name: "Toggle SCIM provisioning" });
    expect(toggle).toBeInTheDocument();
    expect(toggle).not.toBeChecked();
  });

  it("renders the Toggle SCIM provisioning switch (role=switch) when enabled", () => {
    render(<ScimConfigCard connection={baseConnection({ scimEnabled: true })} onChanged={() => {}} />);
    const toggle = screen.getByRole("switch", { name: "Toggle SCIM provisioning" });
    expect(toggle).toBeInTheDocument();
    expect(toggle).toBeChecked();
  });

  it("shows the SCIM disabled status pill even when SCIM is off", () => {
    render(<ScimConfigCard connection={baseConnection({ scimEnabled: false })} onChanged={() => {}} />);
    expect(screen.getByText("SCIM disabled")).toBeInTheDocument();
  });
});
