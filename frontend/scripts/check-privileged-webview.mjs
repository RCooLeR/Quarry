import { readdir, readFile } from "node:fs/promises";
import { dirname, extname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const frontendRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repositoryRoot = resolve(frontendRoot, "..");
const sourceRoot = join(frontendRoot, "src");

function fail(message) {
  throw new Error(`privileged WebView policy: ${message}`);
}

async function sourceFiles(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...await sourceFiles(path));
    } else if (entry.isFile() && [".js", ".jsx", ".ts", ".tsx"].includes(extname(entry.name))) {
      files.push(path);
    }
  }
  return files;
}

const goHeaders = await readFile(join(repositoryRoot, "security_headers.go"), "utf8");
const policyMatch = goHeaders.match(/const\s+contentSecurityPolicy\s*=\s*"([^"]+)"/);
if (!policyMatch) fail("could not read the canonical Go CSP constant");
const canonicalPolicy = policyMatch[1];
// CSP forbids frame-ancestors in a meta-delivered policy. The response header
// remains authoritative and includes it; the document fallback must match all
// other directives exactly.
const canonicalMetaPolicy = canonicalPolicy
  .split(";")
  .map((directive) => directive.trim())
  .filter((directive) => directive !== "" && !directive.startsWith("frame-ancestors "))
  .join("; ");

const indexHTML = await readFile(join(frontendRoot, "index.html"), "utf8");
const metaPolicies = [...indexHTML.matchAll(/<meta\s+[^>]*http-equiv="Content-Security-Policy"[^>]*content="([^"]+)"[^>]*>/gi)];
if (metaPolicies.length !== 1) {
  fail(`expected exactly one CSP meta element, found ${metaPolicies.length}`);
}
if (metaPolicies[0][1] !== canonicalMetaPolicy) {
  fail("HTML CSP differs from the canonical response-header policy");
}
if (/<meta\s+[^>]*http-equiv=["']?refresh\b/i.test(indexHTML)) {
  fail("index.html must not contain meta-refresh navigation");
}

const scripts = [...indexHTML.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)];
if (scripts.length !== 1 || !/\bsrc=["'][^"']+["']/.test(scripts[0][1]) || scripts[0][2].trim() !== "") {
  fail("index.html must contain exactly one external script and no inline script");
}

const mainGo = await readFile(join(repositoryRoot, "main.go"), "utf8");
if (!/Middleware:\s*securityHeaders\b/.test(mainGo)) {
  fail("the Wails asset server is not wired through securityHeaders");
}
if (!/DefaultContextMenuDisabled:\s*true\b/.test(mainGo)) {
  fail("the privileged WebView default browser context menu must stay disabled");
}

const frontendMain = await readFile(join(sourceRoot, "main.tsx"), "utf8");
if (!/import\s*\{\s*installNavigationPolicy\s*\}\s*from\s*["']\.\/navigationPolicy["']/.test(frontendMain)) {
  fail("main.tsx does not import the privileged WebView navigation policy");
}
if (!/\binstallNavigationPolicy\s*\(\s*\)/.test(frontendMain)) {
  fail("main.tsx does not install the privileged WebView navigation policy before rendering");
}
const navigationPolicy = await readFile(join(sourceRoot, "navigationPolicy.ts"), "utf8");
if (!/addEventListener\(\s*["']contextmenu["']/.test(navigationPolicy)
    || !/removeEventListener\(\s*["']contextmenu["']/.test(navigationPolicy)) {
  fail("the cross-platform DOM context-menu backstop is not installed and removable");
}

const forbidden = [
  ["React raw HTML", /\bdangerouslySetInnerHTML\b/],
  ["HTML assignment", /\b(?:innerHTML|outerHTML)\s*=/],
  ["HTML parser insertion", /\binsertAdjacentHTML\s*\(/],
  ["document.write", /\bdocument\s*\.\s*write(?:ln)?\s*\(/],
  ["dynamic code evaluation", /\b(?:eval\s*\(|new\s+Function\b)/],
  ["javascript URL", /["'`]\s*javascript\s*:/i],
  ["remote URL literal", /["'`]https?:\/\//i],
  ["direct network API", /\b(?:fetch|XMLHttpRequest|WebSocket|EventSource|sendBeacon)\b/],
  ["new-window navigation", /\bwindow\s*\.\s*open\s*\(/],
  ["location assignment", /\b(?:(?:window|document)\s*\.\s*)?location\s*(?:\.\s*href\s*)?=/],
  ["location navigation", /\b(?:(?:window|document)\s*\.\s*)?location\s*\.\s*(?:assign|replace)\s*\(/],
  ["history navigation", /\b(?:(?:window)\s*\.\s*)?history\s*\.\s*(?:pushState|replaceState)\s*\(/],
  ["worker execution", /\b(?:new\s+)?(?:SharedWorker|Worker)\s*\(/],
  ["active embedded markup", /<(?:iframe|frame|object|embed|base|form)\b/i],
  ["meta-refresh markup", /\bhttpEquiv\s*=\s*["']refresh\b/i],
];

for (const file of await sourceFiles(sourceRoot)) {
  const source = await readFile(file, "utf8");
  for (const [label, pattern] of forbidden) {
    if (pattern.test(source)) {
      fail(`${label} is forbidden in ${relative(frontendRoot, file)}`);
    }
  }
}

console.log("Privileged WebView policy checks passed.");
