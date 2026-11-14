import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import type { MockLink } from "@apollo/client/testing";
import DeviceProfiles from "./DeviceProfiles";
import {
  DeleteDeviceProfileDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
} from "@/generated/graphql";

const toastSuccess = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { success: toastSuccess, error: vi.fn() } }));

type PolicyMock = {
  __typename: "ResourcePolicy";
  id: string;
  name: string;
  profiles: { __typename: "DeviceProfile"; id: string }[];
};

// A resource policy using the given profile ids.
function policy(id: string, name: string, profileIds: string[]): PolicyMock {
  return {
    __typename: "ResourcePolicy",
    id,
    name,
    profiles: profileIds.map((p) => ({ __typename: "DeviceProfile" as const, id: p })),
  };
}

const PROFILES = [
  {
    __typename: "DeviceProfile" as const,
    id: "profile-1",
    name: "Managed Linux",
    manualTrust: true,
    allowedPlatforms: ["linux"],
    requirements: [
      {
        __typename: "DeviceProfileRequirement" as const,
        id: "req-1",
        checkId: "linux.firewall.active",
        allowUnsupported: false,
      },
    ],
    policies: [] as PolicyMock[],
  },
];

const CHECKS = [
  {
    __typename: "PostureCheckDescriptor" as const,
    id: "linux.firewall.active",
    label: "Firewall active",
    platform: "linux",
    allowUnsupportedMeaningful: true,
  },
];

function renderWithMocks(profiles = PROFILES, extraMocks: MockLink.MockedResponse[] = []) {
  const mocks = [
    ...extraMocks,
    {
      request: { query: GetDeviceProfilesDocument },
      result: { data: { deviceProfiles: profiles } },
    },
    {
      request: { query: GetSupportedPostureChecksDocument },
      result: { data: { supportedPostureChecks: CHECKS } },
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <DeviceProfiles />
    </MockedProvider>,
  );
}

describe("DeviceProfiles", () => {
  it("renders the profile list once loaded", async () => {
    renderWithMocks();

    expect(await screen.findByText("Managed Linux")).toBeInTheDocument();
  });

  it("shows the empty state when there are no profiles", async () => {
    renderWithMocks([]);

    expect(
      await screen.findByText("No device profiles defined"),
    ).toBeInTheDocument();
  });

  it("opens a confirmation dialog before deleting, and Cancel dismisses it", async () => {
    renderWithMocks();
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    expect(await screen.findByText("Delete device profile")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect(screen.queryByText("Delete device profile")).not.toBeInTheDocument(),
    );
  });

  it("warns about affected policies when deleting an in-use profile (D-4)", async () => {
    const inUseProfile = [
      {
        ...PROFILES[0],
        policies: [policy("pol-1", "Strict Production", ["profile-1"])],
      },
    ];
    renderWithMocks(inUseProfile);
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    expect(await screen.findByText("Delete device profile")).toBeInTheDocument();
    expect(screen.getByText(/This profile is used by 1 policy:/)).toBeInTheDocument();
    expect(screen.getAllByText("Strict Production")).toHaveLength(2);
    expect(
      screen.getByText(/This policy has no other profiles and will become/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Confirm & Delete" }),
    ).toBeInTheDocument();
  });

  it("names only the policies left with no profiles as becoming Any Device", async () => {
    renderWithMocks([
      {
        ...PROFILES[0],
        policies: [
          policy("pol-1", "Only this", ["profile-1"]),
          policy("pol-2", "This or other", ["profile-1", "profile-2"]),
        ],
      },
    ]);
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    await screen.findByText("Delete device profile");
    expect(screen.getByText("Only this, This or other")).toBeInTheDocument();
    const anyDevice = screen.getByText(/will become/);
    expect(anyDevice).toHaveTextContent("This policy has no other profiles");
    expect(anyDevice).toHaveTextContent("Only this");
    expect(anyDevice).not.toHaveTextContent("This or other");
  });

  it("says no policy becomes Any Device when each keeps another profile", async () => {
    renderWithMocks([
      {
        ...PROFILES[0],
        policies: [policy("pol-2", "This or other", ["profile-1", "profile-2"])],
      },
    ]);
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    await screen.findByText("Delete device profile");
    expect(screen.getByText(/none will allow any device/)).toBeInTheDocument();
    expect(screen.queryByText(/will become/)).not.toBeInTheDocument();
  });

  it("reports the server's result after a confirmed delete", async () => {
    toastSuccess.mockClear();
    const deleteMock = {
      request: {
        query: DeleteDeviceProfileDocument,
        variables: { id: "profile-1", confirmInUse: true },
      },
      result: {
        data: {
          deleteDeviceProfile: {
            __typename: "DeviceProfileDeletion",
            removedFrom: [
              { __typename: "ResourcePolicyRef", id: "pol-1", name: "Only this" },
              { __typename: "ResourcePolicyRef", id: "pol-2", name: "This or other" },
            ],
            nowAnyDevice: [
              { __typename: "ResourcePolicyRef", id: "pol-1", name: "Only this" },
            ],
          },
        },
      },
    };
    renderWithMocks(
      [
        {
          ...PROFILES[0],
          policies: [
            policy("pol-1", "Only this", ["profile-1"]),
            policy("pol-2", "This or other", ["profile-1", "profile-2"]),
          ],
        },
      ],
      [deleteMock],
    );
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    await userEvent.click(await screen.findByRole("button", { name: "Confirm & Delete" }));

    await waitFor(() => expect(toastSuccess).toHaveBeenCalled());
    expect(toastSuccess).toHaveBeenCalledWith("Device profile deleted", {
      description: "Removed from Only this, This or other. Now allowing any device: Only this.",
    });
  });

  it("opens the edit modal for the clicked profile", async () => {
    renderWithMocks();
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Edit"));

    expect(
      await screen.findByText("Edit Device Profile"),
    ).toBeInTheDocument();
    // ADR-031: the editor opens with the profile's own platforms switched on.
    expect(
      await screen.findByRole("switch", { name: "Allow Linux devices" }),
    ).toBeChecked();
    expect(screen.getByRole("switch", { name: "Allow Windows devices" })).not.toBeChecked();
  });
});
