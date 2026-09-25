import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MockedProvider } from "@apollo/client/testing/react";
import { CreateResourcePolicyModal } from "./CreateResourcePolicyModal";
import {
  CreateResourcePolicyDocument,
  GetResourcePoliciesDocument,
} from "@/generated/graphql";

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

  // Everything above asserts the form's shape. This asserts the write actually
  // happens: MockedProvider matches on variables, so the mutation only resolves
  // if it was sent with the trimmed name, and onSuccess only fires on the
  // mutation's own onCompleted.
  it("sends createResourcePolicy with the trimmed name", async () => {
    const onSuccess = vi.fn();
    const onOpenChange = vi.fn();

    render(
      <MockedProvider
        mocks={[
          {
            request: {
              query: CreateResourcePolicyDocument,
              variables: { name: "Engineering" },
            },
            result: {
              data: {
                createResourcePolicy: {
                  __typename: "ResourcePolicy" as const,
                  id: "rp-new",
                  name: "Engineering",
                },
              },
            },
          },
          {
            request: { query: GetResourcePoliciesDocument },
            result: { data: { resourcePolicies: [] } },
            maxUsageCount: Infinity,
          },
        ]}
      >
        <CreateResourcePolicyModal
          open
          onOpenChange={onOpenChange}
          onSuccess={onSuccess}
        />
      </MockedProvider>,
    );

    // Padded deliberately — the component trims before sending, and the mock
    // above would not match an untrimmed name.
    await userEvent.type(
      await screen.findByPlaceholderText("Policy Name"),
      "  Engineering  ",
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Create Resource Policy" }),
    );

    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
