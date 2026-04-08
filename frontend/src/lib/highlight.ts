import { refractor } from "refractor/core";

// Register languages we'll commonly see in code reviews.
// Import individually to avoid pulling in the full ~300-language bundle.
import go from "refractor/go";
import typescript from "refractor/typescript";
import javascript from "refractor/javascript";
import tsx from "refractor/tsx";
import jsx from "refractor/jsx";
import python from "refractor/python";
import rust from "refractor/rust";
import java from "refractor/java";
import kotlin from "refractor/kotlin";
import swift from "refractor/swift";
import css from "refractor/css";
import scss from "refractor/scss";
import json from "refractor/json";
import yaml from "refractor/yaml";
import toml from "refractor/toml";
import bash from "refractor/bash";
import sql from "refractor/sql";
import markdown from "refractor/markdown";
import docker from "refractor/docker";
import protobuf from "refractor/protobuf";
import makefile from "refractor/makefile";
import ruby from "refractor/ruby";
import csharp from "refractor/csharp";
import cpp from "refractor/cpp";
import c from "refractor/c";

refractor.register(go);
refractor.register(typescript);
refractor.register(javascript);
refractor.register(tsx);
refractor.register(jsx);
refractor.register(python);
refractor.register(rust);
refractor.register(java);
refractor.register(kotlin);
refractor.register(swift);
refractor.register(css);
refractor.register(scss);
refractor.register(json);
refractor.register(yaml);
refractor.register(toml);
refractor.register(bash);
refractor.register(sql);
refractor.register(markdown);
refractor.register(docker);
refractor.register(protobuf);
refractor.register(makefile);
refractor.register(ruby);
refractor.register(csharp);
refractor.register(cpp);
refractor.register(c);

type HighlightCode = Parameters<typeof refractor.highlight>[0];
type HighlightLanguage = Parameters<typeof refractor.highlight>[1];
type HighlightResult = ReturnType<typeof refractor.highlight>;

/**
 * react-diff-view@3 expects `refractor.highlight()` to return an array of nodes.
 * refractor@5 returns a `{type: "root", children: [...]}` node instead, so unwrap
 * to `.children` while keeping a compatible compile-time type for tokenize().
 */
export const diffViewRefractor = {
  highlight(code: HighlightCode, language: HighlightLanguage): HighlightResult {
    return refractor.highlight(code, language).children as unknown as HighlightResult;
  },
};

const EXT_MAP: Record<string, string> = {
  ".go": "go",
  ".ts": "typescript",
  ".tsx": "tsx",
  ".js": "javascript",
  ".jsx": "jsx",
  ".mjs": "javascript",
  ".cjs": "javascript",
  ".py": "python",
  ".rs": "rust",
  ".java": "java",
  ".kt": "kotlin",
  ".kts": "kotlin",
  ".swift": "swift",
  ".css": "css",
  ".scss": "scss",
  ".json": "json",
  ".yaml": "yaml",
  ".yml": "yaml",
  ".toml": "toml",
  ".sh": "bash",
  ".bash": "bash",
  ".zsh": "bash",
  ".sql": "sql",
  ".md": "markdown",
  ".mdx": "markdown",
  ".dockerfile": "docker",
  ".proto": "protobuf",
  ".rb": "ruby",
  ".cs": "csharp",
  ".cpp": "cpp",
  ".cc": "cpp",
  ".cxx": "cpp",
  ".h": "c",
  ".c": "c",
  ".hpp": "cpp",
  ".makefile": "makefile",
};

// Special filename matches (no extension)
const NAME_MAP: Record<string, string> = {
  Dockerfile: "docker",
  Makefile: "makefile",
  Jenkinsfile: "groovy",
};

/** Returns the refractor language name for a file path, or null if unknown. */
export function languageForPath(path: string): string | null {
  // Check full filename first
  const name = path.split("/").pop() ?? "";
  if (NAME_MAP[name]) return NAME_MAP[name];

  // Check extension
  const dot = name.lastIndexOf(".");
  if (dot >= 0) {
    const ext = name.slice(dot).toLowerCase();
    if (EXT_MAP[ext]) return EXT_MAP[ext];
  }

  return null;
}

export { refractor };
