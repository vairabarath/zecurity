import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import { EditResourcePolicyModal } from "./EditResourcePolicyModal";
import { GetDeviceProfilesDocument } from "@/generated/graphql";

const PROFILES = [
  {
    __typename: "DeviceProfile" as const,
    id: "profile-1",
    name: "Corporate Linux",
    manualTrust: true,
    requirements: [
      {
        __typename: "DeviceProfileRequirement" as const,
        id: "req-1",
        checkId: "linux.firewall.active",
        allowUnsupported: false,
      },
    ],
  },
  {
    __typename: "DeviceProfile" as const,
    id: "profile-2",
    name: "Corporate Windows",
    manualTrust: true,
    requirements: [],
  },
];

const POLICY = {
  id: "rp-1",
  name: "Engineering",
  deviceProfiles: [{ id: "profile-1", name: "Corporate Linux" }],
};

function renderModal(policy: typeof POLICY | null = POLICY, open = true) {
  const mocks = [
    {
      request: { query: GetDeviceProfilesDocument },
      result: { data: { deviceProfiles: PROFILES } },
      maxUsageCount: Infinity,
    },
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <EditResourcePolicyModal
        open={open}
        policy={policy}
        onOpenChange={vi.fn()}
      />
    </MockedProvider>,
  );
}

describe("EditResourcePolicyModal", () => {
  it("renders nothing without a policy", () => {
    const { container } = renderModal(null);

    expect(container).toBeEmptyDOMElement();
  });

  it("preselects the profiles the policy already requires", async () => {
    renderModal();

    const linux = await screen.findByRole("checkbox", { name: /Corporate Linux/ });
    const windows = screen.getByRole("checkbox", { name: /Corporate Windows/ });

    expect(linux).toBeChecked();
    expect(windows).not.toBeChecked();
  });

  // The two semantics an admin is most likely to get backwards are stated in
  // words, and must track the selection as it changes.
  it("describes one profile as a requirement and several as alternatives", async () => {
    renderModal();

    // Await the checkbox, not just the text: the wording renders straight from
    // props while the profile list is still loading.
    const windows = await screen.findByRole("checkbox", {
      name: /Corporate Windows/,
    });
    expect(
      screen.getByText("A device must satisfy this profile."),
    ).toBeInTheDocument();

    await userEvent.click(windows);

    expect(
      screen.getByText("A device must satisfy any one of these 2 profiles."),
    ).toBeInTheDocument();
  });

  it("describes an empty selection as Any Device", async () => {
    renderModal();

    await userEvent.click(
      await screen.findByRole("checkbox", { name: /Corporate Linux/ }),
    );

    expect(
      screen.getByText("No profiles selected — this policy allows Any Device."),
    ).toBeInTheDocument();
  });
});
