import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { App } from "./App";

async function compose() {
  render(<App connect={() => () => {}} />);
  await screen.findByRole("heading", { name: "Clients" });
  const form = document.getElementById("compose");
  if (form === null) {
    throw new Error("missing compose");
  }
  const view = within(form);
  fireEvent.change(view.getByRole("textbox", { name: "Recipients" }), { target: { value: "claude-host" } });
  fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "Check in" } });
  return view;
}

// Gap review A11: the hub limits a message by its UTF-8 bytes. A draft must be judged the same
// way, or one that passes the console fails at the hub with a generic error.
describe("draft size", () => {
  it("counts the body in bytes and refuses one over 16 KiB that is far fewer characters", async () => {
    const view = await compose();
    const send = view.getByRole("button", { name: "Send" });
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
    const send = view.getByRole("button", { name: "Send" });
    expect(view.queryByRole("alert")).toBeNull();
    // 342 characters of a 3-byte character are 1 026 bytes.
    fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "€".repeat(342) } });
    expect(view.getByRole("alert")).toHaveTextContent("1026 / 1024 B");
    expect(send).toBeDisabled();
    fireEvent.change(view.getByRole("textbox", { name: "Subject" }), { target: { value: "€".repeat(341) } });
    expect(view.queryByRole("alert")).toBeNull();
    expect(send).toBeEnabled();
  });
});
