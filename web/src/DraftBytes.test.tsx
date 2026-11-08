import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import userEvent from "@testing-library/user-event";
import { App } from "./App";

async function compose() {
  render(<App connect={() => () => {}} />);
  await screen.findByRole("heading", { name: "Clients" });
  const form = document.getElementById("compose");
  if (form === null) {
    throw new Error("missing compose");
  }
  const view = within(form);
  fireEvent.click(view.getByRole("checkbox", { name: /claude-host/ }));
  fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "Check in" } });
  return view;
}

// Gap review A11: the hub limits a message by its UTF-8 bytes. A draft must be judged the same
// way, or one that passes the console fails at the hub with a generic error.
describe("draft size", () => {
  it("counts the body in bytes and refuses one over 16 KiB that is far fewer characters", async () => {
    const view = await compose();
    const send = view.getByRole("button", { name: "Review" });
    // 4 097 characters of a 4-byte emoji are 16 388 bytes: under the old character count, over the hub's limit.
    fireEvent.change(view.getByRole("textbox", { name: "Message" }), { target: { value: "😀".repeat(4097) } });
    expect(view.getByText("16388 / 16384 B")).toBeVisible();
    expect(send).toBeDisabled();
    fireEvent.change(view.getByRole("textbox", { name: "Message" }), { target: { value: "😀".repeat(4096) } });
    expect(view.getByText("16384 / 16384 B")).toBeVisible();
    expect(send).toBeEnabled();
  });

  it("says why a subject over 1024 bytes cannot be sent", async () => {
    const view = await compose();
    fireEvent.change(view.getByRole("textbox", { name: "Message" }), { target: { value: "Hi" } });
    const send = view.getByRole("button", { name: "Review" });
    expect(view.queryByRole("alert")).toBeNull();
    // 342 characters of a 3-byte character are 1 026 bytes.
    fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "€".repeat(342) } });
    expect(view.getByRole("alert")).toHaveTextContent("1026 / 1024 B");
    expect(send).toBeDisabled();
    fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "€".repeat(341) } });
    expect(view.queryByRole("alert")).toBeNull();
    expect(send).toBeEnabled();
  });
  // Ctrl+Enter submits the form even past a disabled button, so the form itself must refuse to
  // open a review of a draft the hub would not take.
  it("does not open a review of a draft that cannot be sent, in compose or reply", async () => {
    const user = userEvent.setup();
    const view = await compose();
    await user.type(view.getByRole("textbox", { name: "Message" }), "{Control>}{Enter}{/Control}");
    expect(view.queryByRole("heading", { name: "Check before sending" })).toBeNull();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = within(await screen.findByRole("article", { name: "Rollout check" }));
    await user.clear(detail.getByRole("textbox", { name: "Recipients" }));
    await user.type(detail.getByRole("textbox", { name: "Message" }), "x{Control>}{Enter}{/Control}");
    expect(detail.queryByRole("heading", { name: "Check before sending" })).toBeNull();
  });
});
