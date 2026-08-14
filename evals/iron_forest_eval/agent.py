from __future__ import annotations

import json
import shlex
from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class IronForestAgent(BaseAgent):
    """Run one production Forest declaration inside a Harbor task sandbox."""

    def __init__(self, logs_dir: Path, model_name: str | None = None, **kwargs):
        super().__init__(logs_dir=logs_dir, model_name=model_name, **kwargs)
        self._scenario_id = ""

    @staticmethod
    @override
    def name() -> str:
        return "iron-forest"

    @override
    def version(self) -> str:
        return "1"

    @override
    async def setup(self, environment: BaseEnvironment) -> None:
        scenario = await environment.exec(command="cat /workspace/scenario.json", timeout_sec=None)
        if scenario.return_code != 0:
            raise RuntimeError(scenario.stderr or scenario.stdout or "missing evaluation scenario")
        try:
            self._scenario_id = json.loads(scenario.stdout)["id"]
        except (json.JSONDecodeError, KeyError, TypeError) as error:
            raise RuntimeError("invalid evaluation scenario") from error
        created = await environment.exec(command="mkdir -p /eval", timeout_sec=None)
        if created.return_code != 0:
            raise RuntimeError(created.stderr or created.stdout or "could not create /eval")
        model_arg = ""
        if self.model_name:
            model_arg = " --model " + shlex.quote(self.model_name)
        result = await environment.exec(
            command="python3 /opt/iron-forest-eval/setup.py /workspace/scenario.json" + model_arg,
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
            command='forest once "$(cat /eval/role)"',
            timeout_sec=None,
        )
        (self.logs_dir / "forest-stdout.txt").write_text(result.stdout or "")
        (self.logs_dir / "forest-stderr.txt").write_text(result.stderr or "")
        run_logs = await environment.exec(command="test -d /workspace/.forest/runs", timeout_sec=None)
        if run_logs.return_code == 0:
            await environment.download_dir("/workspace/.forest/runs", self.logs_dir / "runs")
        await environment.exec(
            command=f"printf '%s\n' {shlex.quote(str(result.return_code))} > /eval/forest-exit",
            timeout_sec=None,
        )
        context.metadata = {
            "forest_exit": result.return_code,
            "scenario": self._scenario_id,
        }
