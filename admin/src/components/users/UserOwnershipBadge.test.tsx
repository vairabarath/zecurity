import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { UserOwnershipBadge } from "./UserOwnershipBadge";

describe("UserOwnershipBadge", () => {
  // ADR-025 §5: "managed by the directory" is derived from provisioningOwner
  // === 'scim', never from provider. The IdP provider only supplies the label.
  it("renders 'Managed by <Provider>' for each known SCIM provider", () => {
    const known: Array<[string, string]> = [
      ["okta", "Okta"],
      ["entra", "Microsoft Entra"],
      ["microsoft-entra", "Microsoft Entra"],
      ["google", "Google Workspace"],
      ["google-workspace", "Google Workspace"],
      ["jumpcloud", "JumpCloud"],
      ["keycloak", "Keycloak"],
      ["generic", "the directory"],
    ];

    for (const [provider, label] of known) {
      const { unmount } = render(
        <UserOwnershipBadge provisioningOwner="scim" provider={provider} />,
      );
      expect(screen.getByText(`Managed by ${label}`)).toBeInTheDocument();
      unmount();
    }
  });

  it("hides entirely when provisioningOwner is not 'scim'", () => {
    for (const owner of ["jit", "manual", "unmanaged"]) {
      const { container, unmount } = render(
        <UserOwnershipBadge provisioningOwner={owner} provider="okta" />,
      );
      expect(container).toBeEmptyDOMElement();
      unmount();
    }
  });

  // The critical ADR-025 rule: a non-scim owner must never badge, even if the
  // provider is a directory IdP. And a scim owner must badge even with a
  // non-directory provider string.
  it("keys off provisioningOwner, not provider", () => {
    const manual = render(
      <UserOwnershipBadge provisioningOwner="manual" provider="okta" />,
    );
    expect(manual.container).toBeEmptyDOMElement();
    manual.unmount();

    render(<UserOwnershipBadge provisioningOwner="scim" provider="okta" />);
    expect(screen.getByText("Managed by Okta")).toBeInTheDocument();
  });

  it("falls back to a capitalized provider name for unknown providers", () => {
    render(<UserOwnershipBadge provisioningOwner="scim" provider="acme" />);
    expect(screen.getByText("Managed by Acme")).toBeInTheDocument();
  });

  it("is case-insensitive when resolving the provider label", () => {
    const upper = render(
      <UserOwnershipBadge provisioningOwner="scim" provider="OKTA" />,
    );
    expect(screen.getByText("Managed by Okta")).toBeInTheDocument();
    upper.unmount();

    const spaced = render(
      <UserOwnershipBadge
        provisioningOwner="scim"
        provider="GOOGLE-WORKSPACE"
      />,
    );
    expect(
      screen.getByText("Managed by Google Workspace"),
    ).toBeInTheDocument();
    spaced.unmount();
  });

  it("renders through StatusPill (status-pill container)", () => {
    const { container } = render(
      <UserOwnershipBadge provisioningOwner="scim" provider="okta" />,
    );
    expect(container.querySelector(".status-pill")).toBeInTheDocument();
  });
});
