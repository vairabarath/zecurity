import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import { CreateDeviceProfileModal } from "./CreateDeviceProfileModal";
import {
  AddProfileRequirementDocument,
  CreateDeviceProfileDocument,
  GetSupportedPostureChecksDocument,
  SetProfilePlatformsDocument,
} from "@/generated/graphql";

vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn() }) }));

const CHECKS = [
  {
    __typename: "PostureCheckDescriptor" as const,
    id: "linux.disk_encryption.luks",
    label: "Disk encryption (LUKS)",
    platform: "linux",
    allowUnsupportedMeaningful: true,
  },
  {
    __typename: "PostureCheckDescriptor" as const,
    id: "linux.firewall.active",
    label: "Firewall active",
    platform: "linux",
    allowUnsupportedMeaningful: true,
  },
  {
    __typename: "PostureCheckDescriptor" as const,
    id: "zecurity.device.approved",
    label: "Approved by an admin",
    platform: "any",
    allowUnsupportedMeaningful: false,
  },
];

function renderModal() {
  const mocks = [
    {
      request: { query: GetSupportedPostureChecksDocument },
      result: { data: { supportedPostureChecks: CHECKS } },
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <CreateDeviceProfileModal open onOpenChange={vi.fn()} />
    </MockedProvider>,
  );
}

describe("CreateDeviceProfileModal", () => {
  it("shows the same platform cards as Edit: every platform but macOS can be turned on", async () => {
    renderModal();
    for (const label of ["Windows", "Linux", "iOS", "Android"]) {
      expect(await screen.findByRole("switch", { name: `Allow ${label} devices` })).toBeEnabled();
    }
    expect(screen.queryByRole("switch", { name: "Allow macOS devices" })).not.toBeInTheDocument();
    expect(screen.getByRole("group", { name: "macOS posture" })).toHaveTextContent("Coming soon");
    expect(screen.queryByText("Soon")).not.toBeInTheDocument();
  });

  it("disables Create until there is a name and at least one platform", async () => {
    renderModal();
    const createButton = screen.getByRole("button", { name: "Create Profile" });
    expect(createButton).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("Profile Name"), "Corporate Laptops");
    expect(createButton).toBeDisabled();
    await userEvent.click(await screen.findByRole("switch", { name: "Allow Linux devices" }));
    expect(createButton).toBeEnabled();
  });

  it("lists a platform's posture checks once it is turned on", async () => {
    renderModal();
    await userEvent.click(await screen.findByRole("switch", { name: "Allow Linux devices" }));
    expect(screen.getByRole("button", { name: "Disk encryption (LUKS) not required" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Firewall active not required" })).toBeInTheDocument();
  });

  it("offers Approved by an admin as an optional requirement, off by default", async () => {
    renderModal();
    const approval = await screen.findByRole("switch", { name: "Approved by an admin" });
    expect(approval).toHaveAttribute("aria-checked", "false");
    await userEvent.click(approval);
    expect(approval).toHaveAttribute("aria-checked", "true");
    expect(screen.getAllByText("Approved by an admin")).toHaveLength(1);
  });

  it("adds the requirements of enabled platforms, then turns the platforms on (ADR-031)", async () => {
    const calls: string[] = [];
    const record = (label: string, data: Record<string, unknown>) => () => {
      calls.push(label);
      return { data };
    };
    const onSuccess = vi.fn();
    render(
      <MockedProvider
        mocks={[
          {
            request: { query: GetSupportedPostureChecksDocument },
            result: { data: { supportedPostureChecks: CHECKS } },
          },
          {
            request: { query: CreateDeviceProfileDocument, variables: { name: "Laptops", manualTrust: true } },
            result: record("create", {
              createDeviceProfile: { __typename: "DeviceProfile", id: "new-1", name: "Laptops", manualTrust: true },
            }),
          },
          {
            request: {
              query: AddProfileRequirementDocument,
              variables: { profileId: "new-1", checkId: "linux.firewall.active", allowUnsupported: false },
            },
            result: record("add firewall", { addProfileRequirement: { __typename: "DeviceProfile", id: "new-1" } }),
          },
          {
            request: { query: SetProfilePlatformsDocument, variables: { profileId: "new-1", platforms: ["linux"] } },
            result: record("platforms linux", {
              setProfilePlatforms: { __typename: "DeviceProfile", id: "new-1", allowedPlatforms: ["linux"] },
            }),
          },
        ]}
      >
        <CreateDeviceProfileModal open onOpenChange={vi.fn()} onSuccess={onSuccess} />
      </MockedProvider>,
    );
    await userEvent.type(screen.getByPlaceholderText("Profile Name"), "Laptops");
    await userEvent.click(await screen.findByRole("switch", { name: "Allow Linux devices" }));
    await userEvent.click(screen.getByRole("button", { name: "Firewall active not required" }));
    await userEvent.click(screen.getByRole("button", { name: "Disk encryption (LUKS) not required" }));
    await userEvent.click(screen.getByRole("button", { name: "Disk encryption (LUKS) required" }));
    await userEvent.click(screen.getByRole("button", { name: "Create Profile" }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(calls).toEqual(["create", "add firewall", "platforms linux"]);
  });
});
