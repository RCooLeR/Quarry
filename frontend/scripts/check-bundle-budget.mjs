import { gzipSync } from "node:zlib";
import { readdir, readFile, stat } from "node:fs/promises";
import { dirname, join, normalize, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const frontendRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const distRoot = join(frontendRoot, "dist");

const limits = Object.freeze({
  entryJavaScriptBytes: 280 * 1024,
  entryJavaScriptGzipBytes: 90 * 1024,
  initialJavaScriptBytes: 320 * 1024,
  initialJavaScriptGzipBytes: 110 * 1024,
  initialStylesheetBytes: 64 * 1024,
  largestJavaScriptChunkBytes: 320 * 1024,
  totalJavaScriptBytes: 720 * 1024,
});

function fail(message) {
  throw new Error(`bundle budget: ${message}`);
}

function resolveDistAsset(publicPath) {
  if (typeof publicPath !== "string" || !publicPath.startsWith("/")) {
    fail(`invalid generated asset path ${JSON.stringify(publicPath)}`);
  }
  const candidate = resolve(distRoot, `.${normalize(publicPath)}`);
  const rel = relative(distRoot, candidate);
  if (rel === "" || rel === ".." || rel.startsWith(`..${sep}`)) {
    fail(`generated asset escapes dist: ${JSON.stringify(publicPath)}`);
  }
  return candidate;
}

async function listFiles(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...await listFiles(path));
    } else if (entry.isFile()) {
      files.push(path);
    } else {
      fail(`unexpected non-regular build artifact: ${relative(distRoot, path)}`);
    }
  }
  return files;
}

const indexPath = join(distRoot, "index.html");
const indexHtml = await readFile(indexPath, "utf8");
const entryScripts = [...indexHtml.matchAll(/<script\b[^>]*\btype=["']module["'][^>]*\bsrc=["']([^"']+)["'][^>]*><\/script>/gi)]
  .map((match) => match[1]);
if (entryScripts.length !== 1) {
  fail(`expected exactly one module entry script, found ${entryScripts.length}`);
}

const initialStylesheets = [...indexHtml.matchAll(/<link\b[^>]*\brel=["']stylesheet["'][^>]*\bhref=["']([^"']+)["'][^>]*>/gi)]
  .map((match) => match[1]);
if (initialStylesheets.length > 1) {
  fail(`expected at most one initial stylesheet, found ${initialStylesheets.length}`);
}

const entryPath = resolveDistAsset(entryScripts[0]);
const entryInfo = await stat(entryPath);
if (!entryInfo.isFile()) fail("module entry is not a regular file");
if (entryInfo.size > limits.entryJavaScriptBytes) {
  fail(`entry JavaScript is ${entryInfo.size} bytes; limit is ${limits.entryJavaScriptBytes}`);
}

const entryBytes = await readFile(entryPath);
const entryGzipBytes = gzipSync(entryBytes, { level: 9 }).byteLength;
if (entryGzipBytes > limits.entryJavaScriptGzipBytes) {
  fail(`gzipped entry JavaScript is ${entryGzipBytes} bytes; limit is ${limits.entryJavaScriptGzipBytes}`);
}

const modulePreloads = [...indexHtml.matchAll(/<link\b[^>]*\brel=["']modulepreload["'][^>]*\bhref=["']([^"']+)["'][^>]*>/gi)]
  .map((match) => match[1]);
const initialScriptPaths = [...new Set([entryScripts[0], ...modulePreloads])];
let initialJavaScriptBytes = 0;
let initialJavaScriptGzipBytes = 0;
for (const publicPath of initialScriptPaths) {
  const scriptPath = resolveDistAsset(publicPath);
  const info = await stat(scriptPath);
  if (!info.isFile()) fail(`initial script is not a regular file: ${publicPath}`);
  initialJavaScriptBytes += info.size;
  const bytes = scriptPath === entryPath ? entryBytes : await readFile(scriptPath);
  initialJavaScriptGzipBytes += gzipSync(bytes, { level: 9 }).byteLength;
}
if (initialJavaScriptBytes > limits.initialJavaScriptBytes) {
  fail(`initial JavaScript is ${initialJavaScriptBytes} bytes; limit is ${limits.initialJavaScriptBytes}`);
}
if (initialJavaScriptGzipBytes > limits.initialJavaScriptGzipBytes) {
  fail(`gzipped initial JavaScript is ${initialJavaScriptGzipBytes} bytes; limit is ${limits.initialJavaScriptGzipBytes}`);
}

let initialStylesheetBytes = 0;
if (initialStylesheets.length === 1) {
  const stylesheetPath = resolveDistAsset(initialStylesheets[0]);
  const stylesheetInfo = await stat(stylesheetPath);
  if (!stylesheetInfo.isFile()) fail("initial stylesheet is not a regular file");
  initialStylesheetBytes = stylesheetInfo.size;
  if (initialStylesheetBytes > limits.initialStylesheetBytes) {
    fail(`initial stylesheet is ${initialStylesheetBytes} bytes; limit is ${limits.initialStylesheetBytes}`);
  }
}

const files = await listFiles(join(distRoot, "assets"));
let totalJavaScriptBytes = 0;
let largestJavaScriptChunkBytes = 0;
for (const file of files) {
  if (!file.endsWith(".js")) continue;
  const info = await stat(file);
  totalJavaScriptBytes += info.size;
  largestJavaScriptChunkBytes = Math.max(largestJavaScriptChunkBytes, info.size);
}

if (largestJavaScriptChunkBytes > limits.largestJavaScriptChunkBytes) {
  fail(`largest JavaScript chunk is ${largestJavaScriptChunkBytes} bytes; limit is ${limits.largestJavaScriptChunkBytes}`);
}
if (totalJavaScriptBytes > limits.totalJavaScriptBytes) {
  fail(`total JavaScript is ${totalJavaScriptBytes} bytes; limit is ${limits.totalJavaScriptBytes}`);
}

console.log(JSON.stringify({
  entryJavaScriptBytes: entryInfo.size,
  entryJavaScriptGzipBytes: entryGzipBytes,
  initialJavaScriptBytes,
  initialJavaScriptGzipBytes,
  initialModulePreloadCount: modulePreloads.length,
  initialStylesheetBytes,
  largestJavaScriptChunkBytes,
  totalJavaScriptBytes,
  limits,
}));
