// Linkifier for tracker / PR references. Operates on markdown source
// rather than the rendered tree: cheap, no remark/rehype plumbing,
// and the resulting markdown autolinks pick up react-markdown's
// default link styling for free.
//
// Skips two regions to avoid re-linkifying or breaking code:
//   - Fenced code blocks (```)
//   - Inline code (`...`)
// Pre-existing markdown links of the form `[X](url)` are also left
// alone via a "followed by ](" lookahead in the match site.

export interface LinkifyContext {
  /** Configured Jira base URL, e.g. "https://acme.atlassian.net".
   *  When unset, Jira keys still highlight stylistically but don't
   *  link out. */
  jiraBaseURL?: string
}

const FENCED_CODE = /(```[\s\S]*?```)/g
const INLINE_CODE = /(`[^`\n]+`)/g

// owner/repo#NNN — GitHub PR or issue. We can't tell which from text
// alone; both URLs work for either kind because GitHub redirects PRs
// from /issues/{n} to /pull/{n} and vice versa, so /issues/{n} is the
// safer canonical default that doesn't 404 on issues.
//
// The trailing `(?![\w-])` is load-bearing: without it `repo#123abc`
// would match `repo#123`, leaving a stranded `abc` after the inserted
// markdown link. The negative-lookahead requires the digit run to end
// at a non-word-char.
const PR_REF = /(?<![[\w-])([A-Za-z0-9][\w.-]*\/[\w.-]+)#(\d+)(?!\]\()(?![\w-])/g

// PROJECT-NNN — Jira key shape. Constrained to uppercase to avoid
// false positives on words like "Pre-2024".
const JIRA_REF = /(?<![[\w-])([A-Z][A-Z0-9]{1,9})-(\d+)(?!\]\()(?![\w-])/g

export function linkifyMarkdown(content: string, ctx: LinkifyContext): string {
  if (!content) return content
  // Outer split on fenced code blocks; inner split on inline code.
  // Replacements only fire on the segments between code regions.
  return content
    .split(FENCED_CODE)
    .map((segment) => {
      if (segment.startsWith('```')) return segment
      return segment
        .split(INLINE_CODE)
        .map((inner) => {
          if (inner.startsWith('`')) return inner
          return applyReplacements(inner, ctx)
        })
        .join('')
    })
    .join('')
}

function applyReplacements(text: string, ctx: LinkifyContext): string {
  // Order matters: PR before Jira. owner/repo#NNN can't accidentally
  // match the Jira shape (Jira requires UPPERCASE then dash), but if
  // both somehow overlapped we'd want the more specific PR shape to
  // win.
  let out = text.replace(PR_REF, (_match, slug: string, num: string) => {
    return `[${slug}#${num}](https://github.com/${slug}/issues/${num})`
  })

  if (ctx.jiraBaseURL) {
    const base = ctx.jiraBaseURL.replace(/\/+$/, '')
    out = out.replace(JIRA_REF, (_match, project: string, num: string) => {
      return `[${project}-${num}](${base}/browse/${project}-${num})`
    })
  }
  return out
}
