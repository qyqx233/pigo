// A remark plugin that turns a literal <br> into a real line break.
//
// Why this exists: a Markdown table row cannot contain a newline, so models
// write <br> inside cells whenever a cell holds a list or several lines. By
// default react-markdown escapes raw HTML, so those tags reach the page as the
// visible text "<br>" and the cell becomes one long run-on line.
//
// Why not rehype-raw: that would render *all* HTML the model emits, and the
// text we render is not trusted input — it is model output that routinely
// quotes tool results, file contents and fetched web pages. One <img onerror>
// in any of those is an XSS. This plugin instead rewrites exactly one node
// type: an html node whose entire value is a <br> tag. Everything else about
// raw HTML stays escaped, so the safety property is unchanged.
//
// Code is untouched: fenced and inline code are leaf nodes with no children, so
// the walk never descends into them and a <br> written inside a code sample
// still shows as written.

// Minimal structural types. The real mdast types live in a transitive
// dependency, and the plugin only needs these three fields.
type MdastNode = {
  type: string;
  value?: string;
  children?: MdastNode[];
};

// Matches a <br> tag and nothing else: the node's whole value must be the tag.
const BR_TAG = /^<br\s*\/?>$/i;

function rewrite(node: MdastNode): void {
  if (!node.children) return;
  for (const child of node.children) {
    if (child.type === "html" && BR_TAG.test((child.value ?? "").trim())) {
      // mdast's "break" is a hard line break; remark-rehype renders it as <br>.
      child.type = "break";
      delete child.value;
      continue;
    }
    rewrite(child);
  }
}

export function remarkLiteralBreaks() {
  return (tree: MdastNode): void => {
    rewrite(tree);
  };
}
