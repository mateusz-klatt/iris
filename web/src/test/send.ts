import type { BoundFunctions, queries } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

/**
 * reviewAndSend sends a draft the way an operator does: Review opens the check of recipients,
 * subject and body, and only its button, labelled for the form, sends. When the review is
 * already open, as after a failed send, it clicks only that button.
 */
export async function reviewAndSend(user: UserEvent, scope: BoundFunctions<typeof queries>, label: string) {
  const review = scope.queryByRole("button", { name: "Review" });
  if (review !== null) {
    await user.click(review);
  }
  await user.click(scope.getByRole("button", { name: label }));
}
