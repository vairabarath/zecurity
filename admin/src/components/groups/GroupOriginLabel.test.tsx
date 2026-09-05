import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { GroupOriginLabel } from "./GroupOriginLabel";

describe("GroupOriginLabel", () => {
  it("renders name + Local suffix for manual origin", () => {
    render(
      <GroupOriginLabel group={{ name: "Engineering", origin: "manual" }} />,
    );
    expect(screen.getByText("Engineering")).toBeInTheDocument();
    expect(screen.getByText(/· Local/)).toBeInTheDocument();
  });

  it("renders name + System suffix for system origin", () => {
    render(<GroupOriginLabel group={{ name: "Admins", origin: "system" }} />);
    expect(screen.getByText(/· System/)).toBeInTheDocument();
  });

  it("renders name + SCIM suffix for scim origin without a resolved connection", () => {
    render(<GroupOriginLabel group={{ name: "Sync", origin: "scim", connectionId: "c1" }} />);
    expect(screen.getByText(/· SCIM/)).toBeInTheDocument();
    expect(screen.queryByText(/\(/)).not.toBeInTheDocument();
  });

  it("appends the resolved connection name for scim origin", () => {
    render(
      <GroupOriginLabel
        group={{ name: "Sync", origin: "scim", connectionId: "c1" }}
        connectionName="Okta"
      />,
    );
    expect(screen.getByText(/· SCIM \(Okta\)/)).toBeInTheDocument();
  });

  it("does NOT append connection name when origin is not scim", () => {
    render(
      <GroupOriginLabel
        group={{ name: "Local Only", origin: "manual" }}
        connectionName="Okta"
      />,
    );
    expect(screen.getByText(/· Local/)).toBeInTheDocument();
    expect(screen.queryByText(/\(Okta\)/)).not.toBeInTheDocument();
  });

  it("falls back to the raw origin string for an unrecognised value", () => {
    render(
      <GroupOriginLabel group={{ name: "Weird", origin: "pending-import" }} />,
    );
    expect(screen.getByText(/· pending-import/)).toBeInTheDocument();
  });
});
