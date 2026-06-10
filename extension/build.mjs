import { build } from "esbuild";

// Production: __TEST_CONFIG__ is null. The UAT harness sets
// BROWSERD_TEST_CONFIG to a JSON object {port, token, profile} to build a
// self-configuring test extension. Never ship a build made with it set.
const testConfig = process.env.BROWSERD_TEST_CONFIG || "null";
const outdir = process.env.BROWSERD_EXT_OUTDIR || "dist";

await build({
  entryPoints: ["src/background.ts", "src/options.ts"],
  outdir,
  bundle: true,
  format: "esm",
  target: "chrome116",
  sourcemap: false,
  minify: false,
  define: { __TEST_CONFIG__: testConfig },
});

console.log(`extension built to ${outdir}/ (test config: ${testConfig !== "null"})`);
