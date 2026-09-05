import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MockedProvider } from "@apollo/client/testing/react";
import { EditDeviceProfileModal } from "./EditDeviceProfileModal";
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

const PROFILE = {
  id: "profile-1",
  name: "Managed Linux",
  manualTrust: true,
  requirements: [{ checkId: "linux.firewall.active" }],
};

function renderModal() {
  const mocks = [
    {
      request: { query: GetSupportedPostureChecksDocument },
      result: { data: { supportedPostureChecks: CHECKS } },
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <EditDeviceProfileModal open profile={PROFILE} onOpenChange={vi.fn()} />
    </MockedProvider>,
  );
}

describe("EditDeviceProfileModal", () => {
  it("shows the profile name read-only, not as an editable input", () => {
    renderModal();

    expect(screen.getByText("Managed Linux")).toBeInTheDocument();
    expect(screen.queryByDisplayValue("Managed Linux")).not.toBeInTheDocument();
  });

  it("pre-checks only the posture checks already in the profile's requirements", async () => {
    renderModal();

    const firewallToggle = await screen.findByRole("switch", {
      name: "Firewall active",
    });
    const luksToggle = screen.getByRole("switch", {
      name: "Disk encryption (LUKS)",
    });

    expect(firewallToggle).toHaveAttribute("aria-checked", "true");
    expect(luksToggle).toHaveAttribute("aria-checked", "false");
  });

  it("renders nothing when closed", () => {
    const { container } = render(
      <MockedProvider mocks={[]}>
        <EditDeviceProfileModal
          open={false}
          profile={PROFILE}
          onOpenChange={vi.fn()}
        />
      </MockedProvider>,
    );

    expect(container).toBeEmptyDOMElement();
  });
});
