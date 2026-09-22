import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import { CreateResourcePolicyModal } from "./CreateResourcePolicyModal";

function renderModal(open = true) {
  return render(
    <MockedProvider mocks={[]}>
      <CreateResourcePolicyModal open={open} onOpenChange={vi.fn()} />
    </MockedProvider>,
  );
}

describe("CreateResourcePolicyModal", () => {
  it("renders nothing while closed", () => {
    const { container } = renderModal(false);

    expect(container).toBeEmptyDOMElement();
  });

  it("requires a name before the policy can be created", async () => {
    renderModal();

    const createButton = await screen.findByRole("button", {
      name: "Create Resource Policy",
    });
    expect(createButton).toBeDisabled();

    await userEvent.type(screen.getByPlaceholderText("Policy Name"), "Engineering");

    expect(createButton).toBeEnabled();
  });

  // A new policy has no profiles, which is a valid "Any Device" state rather
  // than an unfinished object. The panel says so, because an empty requirement
  // list otherwise reads as a mistake.
  it("explains that a new policy starts as Any Device", async () => {
    renderModal();

    expect(await screen.findByText("Starts as “Any Device”")).toBeInTheDocument();
  });

  it("does not offer an audit or enforce control", async () => {
    renderModal();
    await screen.findByPlaceholderText("Policy Name");

    expect(screen.queryByText(/audit/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/enforce/i)).not.toBeInTheDocument();
  });
});
