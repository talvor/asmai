// SPDX-License-Identifier: Apache-2.0

// The entry point of AsmAI's Lavish build: lavish-axi compiled with
// `deno compile`, which needs no Node on the host (ADR 0010).
import process from "node:process";

// The notices of the Lavish build, embedded by the build with --include.
const notices = new URL("./LAVISH_NOTICES", import.meta.url);

if (process.argv[2] === "--notices") {
  await Deno.stdout.write(await Deno.readFile(notices));
  Deno.exit(0);
}

// Lavish starts its background server by running process.execPath with the
// path of its server script. In a compiled executable that is the executable
// itself, which would read the path as the HTML file to open. Dropping the
// path restores self-start.
if (/[\\/]server\.mjs$/.test(process.argv[2] ?? "")) {
  process.argv.splice(2, 1);
}
if (process.argv[2] === "server") {
  await import("lavish-axi/dist/server.mjs");
} else {
  await import("lavish-axi/dist/cli.mjs");
}
