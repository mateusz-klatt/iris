import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import "./i18n";
import { MessageBody } from "./MessageBody";

const png = `data:image/png;base64,${window.btoa("\x89PNG\r\n\x1a\nrest")}`;

describe("MessageBody", () => {
  it("demotes headings and renders only canonical inline images", () => {
    render(
      <MessageBody
        body={[
          "# One",
          "## Two",
          "### Three",
          "#### Four",
          "##### Five",
          "###### Six",
          "",
          "```",
          "const n = 1;",
          "```",
          "",
          "| Col |",
          "| --- |",
          "| cell |",
          "",
          `![diagram](${png} "caption")`,
          "",
          `![](${png})`,
          "",
          "![missing](https://tracker.invalid/x.png)",
        ].join("\n")}
      />,
    );

    expect(screen.getByRole("heading", { level: 2, name: "One" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 3, name: "Two" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 4, name: "Three" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 5, name: "Four" })).toBeVisible();
    expect(screen.queryByRole("heading", { level: 5, name: "Five" })).toBeNull();
    expect(screen.getByRole("heading", { level: 6, name: "Five" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 6, name: "Six" })).toBeVisible();

    const code = document.querySelector("pre");
    expect(code).not.toBeNull();
    expect(code).toHaveAccessibleName("Code block");
    expect(code).toHaveTextContent("const n = 1;");

    const table = screen.getByRole("region", { name: "Markdown table" });
    expect(table.querySelector("td")).toHaveTextContent("cell");

    const images = document.querySelectorAll("img");
    expect(images).toHaveLength(2);
    expect(images[0]).toHaveAttribute("src", png);
    expect(images[0]).toHaveAttribute("alt", "diagram");
    expect(images[0]).toHaveAttribute("title", "caption");
    expect(images[1]).toHaveAttribute("alt", "");
    const rejected = screen.getByText("missing");
    expect(rejected).toHaveClass("markdown-image-alt");
    expect(rejected.closest("img")).toBeNull();
  });
});
