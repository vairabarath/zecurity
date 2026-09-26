import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { MockedProvider } from "@apollo/client/testing/react";
import Policies from "./Policies";
import ResourcePolicies from "./ResourcePolicies";
import DeviceProfiles from "./DeviceProfiles";
import {
  GetDeviceProfilesDocument,
  GetResourcePoliciesDocument,
  GetAllResourcesDocument,
} from "@/generated/graphql";

const MOCKS = [
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

// Policies is a routed shell, so the sections are reached by URL rather than by
// clicking a control inside the page. The route table here mirrors App.tsx.
function renderAt(path: string) {
  return render(
    <MockedProvider mocks={MOCKS}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/policies" element={<Policies />}>
            <Route path="resource-policies" element={<ResourcePolicies />} />
            <Route path="device-profiles" element={<DeviceProfiles />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </MockedProvider>,
  );
}

describe("Policies", () => {
  it("keeps the Policies header above whichever section is routed to", async () => {
    renderAt("/policies/resource-policies");

    expect(
      screen.getByRole("heading", { name: "Policies" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByText("No resource policies defined"),
    ).toBeInTheDocument();
  });

  it("renders Device Profiles at its own route", async () => {
    renderAt("/policies/device-profiles");

    expect(
      screen.getByRole("heading", { name: "Policies" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByText("No device profiles defined"),
    ).toBeInTheDocument();
  });

  // Each section is linkable on its own, which is the point of routing them
  // rather than switching a tab in local state: the sidebar can mark the open
  // section active, and a URL survives a refresh.
  it("shows only the routed section, not both at once", async () => {
    renderAt("/policies/device-profiles");
    await screen.findByText("No device profiles defined");

    expect(
      screen.queryByText("No resource policies defined"),
    ).not.toBeInTheDocument();
  });

  // The Sprint 19 plan's tree lists a Sign In Policy, but no backend exists for
  // it, so it is deliberately absent rather than shipped inert.
  it("does not offer a Sign In Policy section", () => {
    renderAt("/policies/resource-policies");

    expect(screen.queryByText("Sign In Policy")).not.toBeInTheDocument();
  });
});
