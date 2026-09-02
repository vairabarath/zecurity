import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { IdentityHealthBadge } from "./IdentityHealthBadge";

describe("IdentityHealthBadge", () => {
  it("renders each backend-derived state verbatim", () => {
    for (const state of ["Healthy", "Delayed", "Disconnected", "Disabled"]) {
      const { unmount } = render(
        <IdentityHealthBadge identityHealth={state} lastSyncAt={null} scimEnabled />,
      );
      expect(screen.getByText(state)).toBeInTheDocument();
      unmount();
    }
  });

  it("hides entirely when the connection is not SCIM-enabled", () => {
    const { container } = render(
      <IdentityHealthBadge identityHealth="Healthy" lastSyncAt={null} scimEnabled={false} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  // identityHealth is String! but the resolver leaves it empty when the SCIM
  // store is unavailable or IdentityHealth errors. That is the failure path, so
  // it must stay legible rather than rendering an empty pill.
  it("shows Unknown rather than a blank pill when health is empty", () => {
    render(<IdentityHealthBadge identityHealth="" lastSyncAt={null} scimEnabled />);
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });

  it("reads 'never synced' when the connection has never synced", () => {
    render(<IdentityHealthBadge identityHealth="Disconnected" lastSyncAt={null} scimEnabled />);
    expect(screen.getByText("never synced")).toBeInTheDocument();
    expect(screen.queryByText(/last synced/)).not.toBeInTheDocument();
  });

  it("shows a relative last-sync time when the connection has synced", () => {
    const oneHourAgo = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    render(<IdentityHealthBadge identityHealth="Healthy" lastSyncAt={oneHourAgo} scimEnabled />);
    expect(screen.getByText(/last synced 1h ago/)).toBeInTheDocument();
  });
});
