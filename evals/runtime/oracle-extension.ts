import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Deliberately oracle-only: an input hook handles the real Runner prompt before
// Pi starts its agent loop. No assistant, tool, provider, or Run event is forged.
export default function oracleExtension(pi: ExtensionAPI) {
  const dataPath = process.env.FOREST_EVAL_ORACLE_DATA;
  const resultPath = process.env.FOREST_EVAL_ORACLE_RESULT;
  if (!dataPath || !resultPath) throw new Error("missing explicit oracle invocation");
  const status = { handled_inputs: 0, model_turns: 0, exit_code: 1 };
  const execution = { kind: "deterministic-oracle", agent_quality_evidence: false };

  function message(receipt: Record<string, unknown>) {
    pi.sendMessage({
      customType: String(receipt.type),
      content: JSON.stringify(receipt),
      details: receipt,
      display: true,
    }, { triggerTurn: false });
  }

  function save() {
    writeFileSync(resultPath!, JSON.stringify(status) + "\n", { mode: 0o600 });
  }

  async function executeOracle(cwd: string) {
    // Keep every oracle/check descendant in Runner's Pi process group. Stream
    // real receipts as they occur, including those before an interrupted Run.
    const child = spawn("/usr/bin/python3", [join(dirname(dataPath!), "oracle.py"), dataPath!], {
      cwd,
      detached: false,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stderr = "";
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk: string) => { stderr += chunk; });
    const finished = new Promise<{ code: number | null; signal: string | null }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code, signal) => resolve({ code, signal }));
    });
    const receipts = async () => {
      for await (const line of createInterface({ input: child.stdout })) {
        if (line.trim()) message(JSON.parse(line));
      }
    };
    try {
      const [result] = await Promise.all([finished, receipts()]);
      return { ...result, stderr };
    } catch (error) {
      child.kill("SIGTERM");
      throw error;
    }
  }

  // An accidental continuation must fail before provider processing, rather than
  // falling back to a model when an oracle operation or extension hook fails.
  pi.on("before_agent_start", () => {
    status.model_turns++;
    status.exit_code = 1;
    save();
    process.stderr.write("oracle refused an unexpected model turn\n");
    process.exit(1);
  });

  pi.on("input", async (_event, ctx) => {
    status.handled_inputs++;
    try {
      if (status.handled_inputs !== 1) throw new Error("oracle expects exactly one Runner prompt");
      message({
        type: "forest_eval_oracle",
        execution,
        run_id: process.env.FOREST_RUN_ID,
        cwd: ctx.cwd,
        model_turns: 0,
        // Runner requires a usage record even without an agent turn. This is
        // explicit oracle accounting, not invented provider-reported usage.
        usage_source: "deterministic-input-hook:no-model-turn",
        usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 },
      });
      const result = await executeOracle(ctx.cwd);
      if (result.stderr) {
        message({ type: "forest_eval_error", stderr: result.stderr, returncode: result.code });
      }
      status.exit_code = result.signal !== null || result.code !== 0 ? 1 : 0;
      message({
        type: "forest_eval_oracle_complete",
        run_id: process.env.FOREST_RUN_ID,
        execution,
        ...status,
        subprocess_returncode: result.code,
        subprocess_signal: result.signal,
      });
    } catch (error) {
      status.exit_code = 1;
      message({ type: "forest_eval_error", error: String(error) });
    } finally {
      save();
    }
    // Never throw through the input hook: Pi may treat extension errors as a
    // recoverable event. The wrapper reads our actual completion status instead.
    return { action: "handled" };
  });
}
