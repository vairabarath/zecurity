import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import Policies from "./Policies";
import {
  GetDeviceProfilesDocument,
  GetResourcePoliciesDocument,
  GetAllResourcesDocument,
} from "@/generated/graphql";

function renderPolicies() {
  const mocks = [
    {
      request: { query: GetResourcePoliciesDocument },
      result: { data: { resourcePolicies: [] } },
      maxUsageCount: Infinity,
    },
    {
      request: { query: GetAllResourcesDocument },
      result: { data: { allResources: [] } },
      maxUsageCount: Infinity,
    },
    {
      request: { query: GetDeviceProfilesDocument },
      result: { data: { deviceProfiles: [] } },
      maxUsageCount: Infinity,
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <Policies />
    </MockedProvider>,
  );
}

describe("Policies", () => {
  it("renders the Policies header and the Resource Policies tab by default", async () => {
    renderPolicies();

    expect(
      screen.getByRole("heading", { name: "Policies" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByText("No resource policies defined"),
    ).toBeInTheDocument();
  });

  it("offers both policy tabs and can switch to Device Profiles", async () => {
    renderPolicies();
    await screen.findByText("No resource policies defined");

    await userEvent.click(
      screen.getByRole("button", { name: "Device Profiles" }),
    );

    expect(
      await screen.findByText("No device profiles defined"),
    ).toBeInTheDocument();
  });

  // The Sprint 19 plan's tree lists a Sign In Policy, but no backend exists for
  // it, so the tab is deliberately absent rather than shipped inert.
  it("does not offer a Sign In Policy tab", () => {
    renderPolicies();

    expect(screen.queryByText("Sign In Policy")).not.toBeInTheDocument();
  });
});
