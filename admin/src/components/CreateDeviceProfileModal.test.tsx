import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import { CreateDeviceProfileModal } from "./CreateDeviceProfileModal";
import { GetSupportedPostureChecksDocument } from "@/generated/graphql";

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
});
