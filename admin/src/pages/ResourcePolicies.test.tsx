import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import ResourcePolicies from "./ResourcePolicies";
import {
  GetResourcePoliciesDocument,
  GetAllResourcesDocument,
} from "@/generated/graphql";

const LINUX_PROFILE = {
  __typename: "DeviceProfile" as const,
  id: "profile-1",
  name: "Corporate Linux",
};

const WINDOWS_PROFILE = {
  __typename: "DeviceProfile" as const,
  id: "profile-2",
  name: "Corporate Windows",
};

function policy(
  id: string,
  name: string,
  deviceProfiles: typeof LINUX_PROFILE[],
  resources: { __typename: "Resource"; id: string; name: string }[] = [],
) {
  return {
    __typename: "ResourcePolicy" as const,
    id,
    name,
    createdAt: "2026-09-13T10:00:00Z",
    updatedAt: "2026-09-13T10:00:00Z",
    deviceProfiles,
    resources,
  };
}

// allResources is only read to work out which resources are still unassigned.
const RESOURCES = [
  {
    __typename: "Resource" as const,
    id: "res-1",
    name: "Billing DB",
    description: null,
    host: "10.0.0.1",
    protocol: "tcp",
    portFrom: 5432,
    portTo: 5432,
    status: "protected",
    errorMessage: null,
    appliedAt: null,
    lastVerifiedAt: null,
    createdAt: "2026-09-13T10:00:00Z",
    shield: null,
    remoteNetwork: { __typename: "RemoteNetwork" as const, id: "rn-1", name: "Prod" },
    groups: [],
  },
];

function renderWithMocks(policies = [policy("rp-1", "Engineering", [LINUX_PROFILE])]) {
  const mocks = [
    {
      request: { query: GetResourcePoliciesDocument },
      result: { data: { resourcePolicies: policies } },
    },
    {
      request: { query: GetAllResourcesDocument },
      result: { data: { allResources: RESOURCES } },
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <ResourcePolicies />
    </MockedProvider>,
  );
}

describe("ResourcePolicies", () => {
  it("renders the policy list once loaded", async () => {
    renderWithMocks();

    expect(await screen.findByText("Engineering")).toBeInTheDocument();
  });

  it("shows the empty state when there are no policies", async () => {
    renderWithMocks([]);

    expect(
      await screen.findByText("No resource policies defined"),
    ).toBeInTheDocument();
  });

  // The single most misreadable state in the model: no profiles is a deliberate
  // "Any Device", not an unfinished policy and not deny-all.
  it("describes a policy with no device profiles as Any Device", async () => {
    renderWithMocks([policy("rp-empty", "Open Access", [])]);

    expect(await screen.findByText("Open Access")).toBeInTheDocument();
    expect(screen.getByText("Any Device")).toBeInTheDocument();
  });

  // Several profiles are alternatives, not a combined requirement.
  it("presents multiple device profiles as OR", async () => {
    renderWithMocks([
      policy("rp-or", "Either Platform", [LINUX_PROFILE, WINDOWS_PROFILE]),
    ]);

    expect(
      await screen.findByText("Corporate Linux or Corporate Windows"),
    ).toBeInTheDocument();
    expect(screen.getByText("any one of 2 profiles")).toBeInTheDocument();
  });

  it("opens a confirmation dialog before deleting, and Cancel dismisses it", async () => {
    renderWithMocks();
    await screen.findByText("Engineering");

    await userEvent.click(screen.getByText("Delete"));
    expect(
      await screen.findByText("Delete resource policy"),
    ).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect(
        screen.queryByText("Delete resource policy"),
      ).not.toBeInTheDocument(),
    );
  });

  it("opens the edit modal for the clicked policy", async () => {
    renderWithMocks();
    await screen.findByText("Engineering");

    await userEvent.click(screen.getByText("Edit"));

    expect(await screen.findByText("Edit Resource Policy")).toBeInTheDocument();
  });

  // The one-policy-per-resource rule shows up in the UI as a filtered candidate
  // list: a resource another policy already claims must not be offered.
  it("only offers resources that have no policy yet", async () => {
    renderWithMocks([
      policy("rp-1", "Engineering", [LINUX_PROFILE], [
        { __typename: "Resource" as const, id: "res-1", name: "Billing DB" },
      ]),
    ]);
    await screen.findByText("Engineering");

    // "Resources" is also a column header, so target the row action by role.
    await userEvent.click(screen.getByRole("button", { name: "Resources" }));

    expect(
      await screen.findByText("Resources using Engineering"),
    ).toBeInTheDocument();
    // res-1 is claimed by this policy, so the picker has no candidates left.
    expect(
      screen.getByText("Every resource already has a policy"),
    ).toBeInTheDocument();
  });
});
