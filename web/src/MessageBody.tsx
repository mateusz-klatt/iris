import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";

import { markdownUrlTransform } from "./markdown";

const remarkPlugins = [remarkGfm, remarkBreaks];

export function MessageBody({ body }: { body: string }) {
  const { t } = useTranslation();
  const components = useMemo<Components>(
    () => ({
      h1({ children }) {
        return <h2>{children}</h2>;
      },
      h2({ children }) {
        return <h3>{children}</h3>;
      },
      h3({ children }) {
        return <h4>{children}</h4>;
      },
      h4({ children }) {
        return <h5>{children}</h5>;
      },
      h5({ children }) {
        return <h6>{children}</h6>;
      },
      h6({ children }) {
        return <h6>{children}</h6>;
      },
      a({ href, children, title }) {
        if (href === undefined) {
          return <span className="markdown-rejected-link">{children}</span>;
        }
        return (
          <a href={href} title={title} rel="noreferrer">
            {children}
          </a>
        );
      },
      img({ src, alt, title }) {
        if (src === undefined) {
          return <span className="markdown-image-alt">{alt}</span>;
        }
        return <img src={src} alt={alt ?? ""} title={title} />;
      },
      pre({ children }) {
        return (
          <pre tabIndex={0} aria-label={t("markdown.codeBlock")}>
            {children}
          </pre>
        );
      },
      table({ children }) {
        return (
          <div className="markdown-table-scroll" role="region" tabIndex={0} aria-label={t("markdown.table")}>
            <table>{children}</table>
          </div>
        );
      },
    }),
    [t],
  );

  return (
    <div className="message-body">
      <ReactMarkdown
        remarkPlugins={remarkPlugins}
        urlTransform={markdownUrlTransform}
        components={components}
      >
        {body}
      </ReactMarkdown>
    </div>
  );
}
