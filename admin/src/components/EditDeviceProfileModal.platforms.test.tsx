import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import type { MockLink } from "@apollo/client/testing";
import { EditDeviceProfileModal } from "./EditDeviceProfileModal";
import {
  AddProfileRequirementDocument,
  GetSupportedPostureChecksDocument,
  SetProfilePlatformsDocument,
} from "@/generated/graphql";

vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn() }) }));

const check = (id: string, label: string, platform: string) => ({
  __typename: "PostureCheckDescriptor" as const,
  id,
  label,
  platform,
  allowUnsupportedMeaningful: false,
});

const CHECKS = [
  check("linux.firewall.active", "Firewall active", "linux"),
  check("android.disk_encryption.enabled", "HD Encryption", "android"),
  check("android.screen_lock.enabled", "Screen Lock", "android"),
  check("android.biometric.enrolled", "Biometric", "android"),
];

function profile(platforms: string[]) {
  return {
    id: "p1",
    name: "Company devices",
    manualTrust: true,
    allowedPlatforms: platforms,
    requirements: [{ checkId: "linux.firewall.active" }],
    policies: [],
  };
}

// Records the order in which mutations reach the server.
function recorder() {
  const calls: string[] = [];
  const mock = (
    query: MockLink.MockedResponse["request"]["query"],
    variables: Record<string, unknown>,
    label: string,
    data: Record<string, unknown>,
  ): MockLink.MockedResponse => ({
    request: { query, variables },
    result: () => {
      calls.push(label);
      return { data };
    },
  });
  return { calls, mock };
}

const platformsResult = (platforms: string[]) => ({
  setProfilePlatforms: { __typename: "DeviceProfile", id: "p1", allowedPlatforms: platforms },
});
const addResult = { addProfileRequirement: { __typename: "DeviceProfile", id: "p1" } };

function renderModal(p: ReturnType<typeof profile>, mocks: MockLink.MockedResponse[]) {
  const onSuccess = vi.fn();
  render(
    <MockedProvider
      mocks={[
        {
          request: { query: GetSupportedPostureChecksDocument },
          result: { data: { supportedPostureChecks: CHECKS } },
        },
        ...mocks,
      ]}
    >
      <EditDeviceProfileModal open profile={p} onOpenChange={() => {}} onSuccess={onSuccess} />
    </MockedProvider>,
  );
  return onSuccess;
}

describe("EditDeviceProfileModal platform cards (ADR-031)", () => {
  it("shows a card per platform; a platform that is off is not allowed", async () => {
    renderModal(profile(["linux"]), []);
    expect(await screen.findByText("Android devices are not allowed by this profile.")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Allow Linux devices" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Allow Android devices" })).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Firewall active required" })).toBeInTheDocument();
  });

  it("turning Android on lists its checks as in the reference card, then requires Screen Lock", async () => {
    const { calls, mock } = recorder();
    const onSuccess = renderModal(profile(["linux"]), [
      mock(AddProfileRequirementDocument,
        { profileId: "p1", checkId: "android.screen_lock.enabled", allowUnsupported: false },
        "add screen lock", addResult),
      mock(SetProfilePlatformsDocument, { profileId: "p1", platforms: ["linux", "android"] },
        "platforms linux+android", platformsResult(["linux", "android"])),
    ]);

    await userEvent.click(await screen.findByRole("switch", { name: "Allow Android devices" }));
    expect(screen.getByRole("button", { name: "HD Encryption not required" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Screen Lock not required" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Biometric not required" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Screen Lock not required" }));
    expect(screen.getByRole("button", { name: "Screen Lock required" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    // The platform is turned on only after its requirement is in place.
    expect(calls).toEqual(["add screen lock", "platforms linux+android"]);
  });

  it("turning a platform off happens before any requirement change", async () => {
    const { calls, mock } = recorder();
    const onSuccess = renderModal(profile(["linux", "android"]), [
      mock(SetProfilePlatformsDocument, { profileId: "p1", platforms: ["android"] },
        "platforms android", platformsResult(["android"])),
      mock(AddProfileRequirementDocument,
        { profileId: "p1", checkId: "android.biometric.enrolled", allowUnsupported: false },
        "add biometric", addResult),
    ]);

    await userEvent.click(await screen.findByRole("switch", { name: "Allow Linux devices" }));
    await userEvent.click(screen.getByRole("button", { name: "Biometric not required" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(calls).toEqual(["platforms android", "add biometric"]);
  });
});
