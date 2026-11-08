import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { DraftBody, DraftReview } from "./DraftReview";
import { useReview } from "./review";
import i18n from "./i18n";

beforeEach(async () => {
  document.documentElement.lang = "en";
  await i18n.changeLanguage("en");
});

// A form like the console's: every edit gives the draft a new key, submit opens the review.
function Form({ onSend, sendable = true, sending = false }: { onSend: (body: string) => void; sendable?: boolean; sending?: boolean }) {
  const [draft, setDraft] = useState({ body: "**hi**", key: "k0" });
  const review = useReview(draft.key);
  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        review.start();
      }}
    >
      <DraftBody value={draft.body} onChange={(body) => setDraft((current) => ({ body, key: `${current.key}+` }))} />
      <DraftReview
        review={review}
        recipients={["alice", "bob"]}
        subject="Rollout"
        body={draft.body}
        sendable={sendable}
        sending={sending}
        sendLabel="Send"
        onSend={() => onSend(draft.body)}
      />
    </form>
  );
}

describe("DraftBody", () => {
  it("previews the draft with the thread's renderer and goes back to editing", async () => {
    const user = userEvent.setup();
    render(<Form onSend={() => {}} />);
    expect(screen.getByRole("button", { name: "Edit" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "Preview" }));
    const preview = screen.getByRole("region", { name: "Preview" });
    expect(preview.querySelector("strong")).toHaveTextContent("hi");
    expect(screen.queryByRole("textbox")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("textbox", { name: "Message" })).toHaveValue("**hi**");
  });

  it("says so when there is nothing to preview", async () => {
    const user = userEvent.setup();
    render(<DraftBody value="  " onChange={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Preview" }));
    expect(screen.getByRole("region", { name: "Preview" })).toHaveTextContent("Nothing to preview yet.");
  });

  it("opens the review on Ctrl+Enter or Command+Enter, and Enter alone is a new line", async () => {
    const user = userEvent.setup();
    render(<Form onSend={() => {}} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    expect(box).toHaveAccessibleDescription("Press Ctrl+Enter or Command+Enter to review.");
    await user.type(box, "{Enter}");
    expect(screen.queryByRole("heading", { name: "Check before sending" })).toBeNull();
    await user.type(box, "{Control>}{Enter}{/Control}");
    expect(screen.getByRole("heading", { name: "Check before sending" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Back to the draft" }));
    await user.type(box, "{Meta>}{Enter}{/Meta}");
    expect(screen.getByRole("heading", { name: "Check before sending" })).toBeInTheDocument();
  });

  it("does nothing on Ctrl+Enter outside a form", async () => {
    const user = userEvent.setup();
    render(<DraftBody value="x" onChange={() => {}} />);
    await user.type(screen.getByRole("textbox"), "{Control>}{Enter}{/Control}");
    expect(screen.getByRole("textbox")).toBeInTheDocument();
  });
});

describe("DraftReview", () => {
  it("shows every recipient, the subject and the rendered body before Send", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Form onSend={onSend} />);
    await user.click(screen.getByRole("button", { name: "Review" }));
    const review = screen.getByRole("region", { name: "Check before sending" });
    expect([...review.querySelectorAll("li")].map((item) => item.textContent)).toEqual(["alice", "bob"]);
    expect(review).toHaveTextContent("Rollout");
    expect(review.querySelector(".message-body strong")).toHaveTextContent("hi");
    const send = screen.getByRole("button", { name: "Send" });
    expect(send).toHaveFocus();
    expect(onSend).not.toHaveBeenCalled();
    await user.click(send);
    expect(onSend).toHaveBeenCalledWith("**hi**");
  });

  it("closes when the draft is edited, so what is sent was reviewed", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Form onSend={onSend} />);
    await user.click(screen.getByRole("button", { name: "Review" }));
    await user.type(screen.getByRole("textbox", { name: "Message" }), "!");
    expect(screen.queryByRole("button", { name: "Send" })).toBeNull();
    expect(screen.getByRole("button", { name: "Review" })).toBeEnabled();
    expect(onSend).not.toHaveBeenCalled();
  });

  it("goes back to editing without sending", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Form onSend={onSend} />);
    await user.click(screen.getByRole("button", { name: "Review" }));
    await user.click(screen.getByRole("button", { name: "Back to the draft" }));
    expect(screen.queryByRole("heading", { name: "Check before sending" })).toBeNull();
    expect(onSend).not.toHaveBeenCalled();
  });

  it("cannot be opened or sent while the draft is not sendable or a send is in flight", () => {
    const { rerender } = render(<Form onSend={() => {}} sendable={false} />);
    expect(screen.getByRole("button", { name: "Review" })).toBeDisabled();
    rerender(<Form onSend={() => {}} sending />);
    expect(screen.getByRole("button", { name: "Review" })).toBeDisabled();
  });

  it("disables Send while sending", async () => {
    const user = userEvent.setup();
    function Sending() {
      const [sending, setSending] = useState(false);
      const review = useReview("k");
      return (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            review.start();
          }}
        >
          <DraftReview review={review} recipients={["a"]} subject="s" body="b" sendable sending={sending} sendLabel="Send" onSend={() => setSending(true)} />
        </form>
      );
    }
    render(<Sending />);
    await user.click(screen.getByRole("button", { name: "Review" }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
  });
});
