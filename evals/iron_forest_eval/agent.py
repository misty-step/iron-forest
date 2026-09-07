from __future__ import annotations

import json
import shlex
import tempfile
from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from iron_forest_eval.usage import usage_from_run_logs

CASES = Path(__file__).resolve().parents[1] / "cases.json"


def case_id_from_logs(logs_dir: Path) -> str:
    return logs_dir.parent.name.split("__", 1)[0]


def scenario_for(case_id: str) -> dict:
    manifest = json.loads(CASES.read_text())
    for case in manifest["cases"]:
        if case["id"] == case_id:
            return case
    raise RuntimeError(f"unknown evaluation case: {case_id}")


class IronForestAgent(BaseAgent):
    """Run one production Forest declaration inside a Harbor task sandbox."""

    def __init__(
        self,
        logs_dir: Path,
        model_name: str | None = None,
        thinking: str | None = None,
        tools: str | None = None,
        prompt_append: str | None = None,
        variant_roles: str | None = None,
        **kwargs,
    ):
        super().__init__(logs_dir=logs_dir, model_name=model_name, **kwargs)
        self._thinking = thinking
        self._tools = tools
        self._prompt_append = prompt_append
        self._variant_roles = {
            role.strip() for role in (variant_roles or "").split(",") if role.strip()
        }
        self._scenario_id = ""
        self._scenario: dict = {}

    @staticmethod
    @override
    def name() -> str:
        return "iron-forest"

    @override
    def version(self) -> str:
        return "1"

    @override
    async def setup(self, environment: BaseEnvironment) -> None:
        case = scenario_for(case_id_from_logs(self.logs_dir))
        self._scenario_id = case["id"]
        self._scenario = case
        created = await environment.exec(command="mkdir -p /hidden", user="root", timeout_sec=None)
        if created.return_code != 0:
            raise RuntimeError(created.stderr or created.stdout or "could not create /hidden")
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
            handle.write(json.dumps(case, indent=2, sort_keys=True) + "\n")
            local_scenario = handle.name
        try:
            await environment.upload_file(local_scenario, "/hidden/scenario.json")
        finally:
            Path(local_scenario).unlink(missing_ok=True)
        declaration_args = []
        applies_variant = not self._variant_roles or case["role"] in self._variant_roles
        if applies_variant:
            for name, value in (
                ("model", self.model_name),
                ("thinking", self._thinking),
                ("tools", self._tools),
                ("prompt-append", self._prompt_append),
            ):
                if value:
                    declaration_args.extend((f"--{name}", value))
        command = "python3 /opt/iron-forest-eval/setup.py /hidden/scenario.json"
        if declaration_args:
            command += " " + shlex.join(declaration_args)
        result = await environment.exec(
            command=command,
            user="root",
            timeout_sec=None,
        )
        if result.return_code != 0:
            raise RuntimeError(result.stderr or result.stdout or "scenario setup failed")

    @override
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        result = await environment.exec(
            command='forest once "$(cat /run/forest-eval/role)"',
            env=self._scenario.get("agent_env") or None,
            timeout_sec=None,
        )
        (self.logs_dir / "forest-stdout.txt").write_text(result.stdout or "")
        (self.logs_dir / "forest-stderr.txt").write_text(result.stderr or "")
        runs_dir = self.logs_dir / "runs"
        run_logs = await environment.exec(command="test -d /workspace/.forest/runs", timeout_sec=None)
        if run_logs.return_code == 0:
            await environment.download_dir("/workspace/.forest/runs", runs_dir)
            usage = usage_from_run_logs(runs_dir)
            context.n_input_tokens = usage["n_input_tokens"]
            context.n_cache_tokens = usage["n_cache_tokens"]
            context.n_output_tokens = usage["n_output_tokens"]
            context.cost_usd = usage["cost_usd"]
        recorded = await environment.exec(
            command=f"printf '%s\\n' {shlex.quote(str(result.return_code))} > /hidden/forest-exit",
            user="root",
            timeout_sec=None,
        )
        if recorded.return_code != 0:
            raise RuntimeError(recorded.stderr or recorded.stdout or "could not record forest exit")
        context.metadata = {
            "forest_exit": result.return_code,
            "scenario": self._scenario_id,
            "usage_source": "forest-run-jsonl",
        }
