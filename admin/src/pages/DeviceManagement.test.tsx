import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import type { MockedResponse } from "@apollo/client/testing";
import DeviceManagement from "./DeviceManagement";
import {
  ApproveDeviceDocument,
  GetClientDevicesDocument,
  UnapproveDeviceDocument,
} from "@/generated/graphql";

function device(id: string, name: string, approvedAt: string | null) {
  return {
    __typename: "ClientDevice" as const,
    id,
    userId: "user-1",
    name,
    commonName: name,
    os: "linux",
    spiffeId: null,
    certNotAfter: null,
    lastSeenAt: null,
    createdAt: "2026-10-28T00:00:00Z",
    revokedAt: null,
    approvedAt,
    approvedByEmail: approvedAt ? "admin@example.test" : null,
  };
}

function approvalResult(id: string, approvedAt: string | null) {
  return {
    __typename: "ClientDevice" as const,
    id,
    approvedAt,
    approvedByEmail: approvedAt ? "admin@example.test" : null,
  };
}

const NOW = "2026-10-28T10:00:00Z";

function renderPage(extraMocks: MockedResponse[] = []) {
  const mocks = [
    {
      request: { query: GetClientDevicesDocument },
      result: {
        data: {
          clientDevices: [
            device("dev-1", "byod-laptop", null),
            device("dev-2", "office-laptop", NOW),
          ],
        },
      },
    },
    ...extraMocks,
  ];
  return render(
    <MockedProvider mocks={mocks}>
      <DeviceManagement />
    </MockedProvider>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("DeviceManagement approval (ADR-030)", () => {
  it("shows each device's approval state", async () => {
    renderPage();

    expect(await screen.findByText("byod-laptop")).toBeInTheDocument();
    expect(screen.getByText("not approved")).toBeInTheDocument();
    expect(screen.getByText("approved")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Remove approval" }),
    ).toBeInTheDocument();
  });

  it("approves a device after confirmation", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const approve = vi.fn(() => ({
      data: { approveDevice: approvalResult("dev-1", NOW) },
    }));
    renderPage([
      {
        request: { query: ApproveDeviceDocument, variables: { deviceId: "dev-1" } },
        result: approve,
      },
    ]);

    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approve).toHaveBeenCalledTimes(1));
    expect(await screen.findAllByText("approved")).toHaveLength(2);
  });

  it("removes an approval after confirmation", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const unapprove = vi.fn(() => ({
      data: { unapproveDevice: approvalResult("dev-2", null) },
    }));
    renderPage([
      {
        request: { query: UnapproveDeviceDocument, variables: { deviceId: "dev-2" } },
        result: unapprove,
      },
    ]);

    await userEvent.click(
      await screen.findByRole("button", { name: "Remove approval" }),
    );

    await waitFor(() => expect(unapprove).toHaveBeenCalledTimes(1));
    expect(await screen.findAllByText("not approved")).toHaveLength(2);
  });

  it("does nothing when the confirmation is declined", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const approve = vi.fn();
    renderPage([
      {
        request: { query: ApproveDeviceDocument, variables: { deviceId: "dev-1" } },
        result: approve,
      },
    ]);

    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));

    expect(approve).not.toHaveBeenCalled();
  });
});
