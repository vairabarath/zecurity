import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import Policies from "./Policies";
import {
  GetDeviceProfilesDocument,
  GetResourcePoliciesDocument,
} from "@/generated/graphql";

function renderPolicies() {
  const mocks = [
    {
      request: { query: GetResourcePoliciesDocument },
      result: {
        data: {
          resourcePolicies: [
            {
              __typename: "ResourcePolicy" as const,
              id: "pol-1",
              name: "Default",
              description: null,
              isDefault: true,
              resourceCount: 0,
              profiles: [],
              resources: [],
            },
          ],
        },
      },
    },
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
  it("renders the Policies header and both tabs, defaulting to Resource Policies", async () => {
    renderPolicies();

    expect(screen.getByRole("heading", { name: "Policies" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resource Policies" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Device Profiles" })).toBeInTheDocument();
    expect((await screen.findAllByText("Default")).length).toBeGreaterThan(0);
  });

  it("switches to Device Profiles when clicked", async () => {
    renderPolicies();

    const deviceProfilesTab = screen.getByText("Device Profiles");
    await userEvent.click(deviceProfilesTab);

    expect(
      await screen.findByText("No device profiles defined"),
    ).toBeInTheDocument();
  });
});
