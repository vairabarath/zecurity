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

  it("renders Manual Trust as always-on and non-interactive", async () => {
    renderModal();
    await userEvent.click(screen.getByRole("button", { name: /Linux/ }));

    const manualTrust = await screen.findByRole("switch", {
      name: "Manual Trust (always enabled)",
    });
    expect(manualTrust).toHaveAttribute("aria-checked", "true");
    expect(manualTrust).toBeDisabled();
  });
});
