import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import DeviceProfiles from "./DeviceProfiles";
import {
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
} from "@/generated/graphql";

const PROFILES = [
  {
    __typename: "DeviceProfile" as const,
    id: "profile-1",
    name: "Managed Linux",
    manualTrust: true,
    requirements: [
      {
        __typename: "DeviceProfileRequirement" as const,
        id: "req-1",
        checkId: "linux.firewall.active",
        allowUnsupported: false,
      },
    ],
    policies: [] as { id: string; name: string }[],
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

function renderWithMocks(profiles = PROFILES) {
  const mocks = [
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
        policies: [{ id: "pol-1", name: "Strict Production" }],
      },
    ];
    renderWithMocks(inUseProfile);
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Delete"));
    expect(await screen.findByText("Delete device profile")).toBeInTheDocument();
    expect(screen.getByText(/This profile is used by 1 policy:/)).toBeInTheDocument();
    expect(screen.getByText("Strict Production")).toBeInTheDocument();
    expect(
      screen.getByText(/Any policy left with no other profiles will become/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Confirm & Delete" }),
    ).toBeInTheDocument();
  });

  it("opens the edit modal for the clicked profile", async () => {
    renderWithMocks();
    await screen.findByText("Managed Linux");

    await userEvent.click(screen.getByText("Edit"));

    expect(
      await screen.findByText("Edit Trusted Linux Profile"),
    ).toBeInTheDocument();
  });
});
