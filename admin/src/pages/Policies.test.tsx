import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MockedProvider } from "@apollo/client/testing/react";
import Policies from "./Policies";
import { GetDeviceProfilesDocument } from "@/generated/graphql";

function renderPolicies() {
  const mocks = [
    {
      request: { query: GetDeviceProfilesDocument },
      result: { data: { deviceProfiles: [] } },
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <Policies />
    </MockedProvider>,
  );
}

describe("Policies", () => {
  it("renders the Policies header and the Device Policies tab's content by default", async () => {
    renderPolicies();

    expect(screen.getByRole("heading", { name: "Policies" })).toBeInTheDocument();
    expect(screen.getByText("Device Policies")).toBeInTheDocument();
    expect(
      await screen.findByText("No device profiles defined"),
    ).toBeInTheDocument();
  });
});
