from __future__ import annotations

import json
import shlex
import tempfile
from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

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

    def __init__(self, logs_dir: Path, model_name: str | None = None, **kwargs):
        super().__init__(logs_dir=logs_dir, model_name=model_name, **kwargs)
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
        model_arg = ""
        if self.model_name:
            model_arg = " --model " + shlex.quote(self.model_name)
        result = await environment.exec(
            command="python3 /opt/iron-forest-eval/setup.py /hidden/scenario.json" + model_arg,
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
        run_logs = await environment.exec(command="test -d /workspace/.forest/runs", timeout_sec=None)
        if run_logs.return_code == 0:
            await environment.download_dir("/workspace/.forest/runs", self.logs_dir / "runs")
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
        }
