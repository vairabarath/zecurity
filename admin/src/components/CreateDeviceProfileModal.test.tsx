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
  it("shows the OS picker first, with only Linux enabled", () => {
    renderModal();

    expect(screen.getByRole("button", { name: /Linux/ })).toBeEnabled();
    expect(screen.getByRole("button", { name: /Windows/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /macOS/ })).toBeDisabled();
  });

  it("reveals the Create Trusted Linux Profile panel after choosing Linux", async () => {
    renderModal();

    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));

    expect(
      await screen.findByText("Create Trusted Linux Profile"),
    ).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Profile Name")).toBeInTheDocument();
  });

  it("disables Create until a profile name is entered", async () => {
    renderModal();
    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));

    const createButton = await screen.findByRole("button", {
      name: "Create Trusted Profile",
    });
    expect(createButton).toBeDisabled();

    await userEvent.type(
      screen.getByPlaceholderText("Profile Name"),
      "Corporate Laptops",
    );
    expect(createButton).toBeEnabled();
  });

  it("lists Linux posture checks sourced from supportedPostureChecks", async () => {
    renderModal();
    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));

    expect(
      await screen.findByText("Disk encryption (LUKS)"),
    ).toBeInTheDocument();
    expect(screen.getByText("Firewall active")).toBeInTheDocument();
  });

  it("offers Approved by an admin as an optional requirement, off by default", async () => {
    renderModal();
    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));

    const approval = await screen.findByRole("switch", {
      name: "Approved by an admin",
    });
    expect(approval).toHaveAttribute("aria-checked", "false");
    expect(approval).toBeEnabled();
    await userEvent.click(approval);
    expect(approval).toHaveAttribute("aria-checked", "true");
    expect(screen.queryByText("Manual Trust")).not.toBeInTheDocument();
    // Server checks are not listed again among the Linux posture checks.
    expect(screen.getAllByText("Approved by an admin")).toHaveLength(1);
  });

  it("turns the chosen platform on after its requirements (ADR-031)", async () => {
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
    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));
    await userEvent.type(screen.getByPlaceholderText("Profile Name"), "Laptops");
    await userEvent.click(await screen.findByRole("switch", { name: "Firewall active" }));
    await userEvent.click(screen.getByRole("button", { name: /^Create/ }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(calls).toEqual(["create", "add firewall", "platforms linux"]);
  });
});
