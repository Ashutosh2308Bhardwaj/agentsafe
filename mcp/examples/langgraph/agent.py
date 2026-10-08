"""A LangGraph agent that bills customers through an MCP server.

There is no agentsafe code here. The agent is built with LangChain's create_agent (a LangGraph graph) and gets
its tools from the MCP servers listed in mcp_servers.json, through langchain-mcp-adapters. Putting agentsafe in
front of the billing server is a change to that file only.

The model is scripted (offline, deterministic, no API key): it asks to charge the ticket, then reports what the
tool answered. Swap in any LangChain chat model, e.g. ChatAnthropic(model="claude-opus-5-5").

    python agent.py T-77 1200.00
"""

import asyncio
import json
import sys
from pathlib import Path
from typing import Any

from langchain.agents import create_agent
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage
from langchain_core.outputs import ChatGeneration, ChatResult
from langchain_mcp_adapters.client import MultiServerMCPClient


class ScriptedModel(BaseChatModel):
    """Asks for one charge, then answers with what the tool said."""

    ticket: str
    amount: str

    @property
    def _llm_type(self) -> str:
        return "scripted"

    def bind_tools(self, tools: Any, **kwargs: Any) -> "ScriptedModel":
        return self

    def _generate(self, messages: list[BaseMessage], stop: Any = None, run_manager: Any = None, **kwargs: Any) -> ChatResult:
        last = messages[-1]
        if last.type == "tool":
            reply = AIMessage(content=f"The billing tool answered: {text(last.content)}")
        else:
            reply = AIMessage(content="", tool_calls=[{"name": "charge", "id": "call_1",
                                                       "args": {"ticket_id": self.ticket, "amount": self.amount}}])
        return ChatResult(generations=[ChatGeneration(message=reply)])


def text(content: Any) -> str:
    """A tool message's text: MCP results arrive as a list of content blocks."""
    if isinstance(content, list):
        return "".join(b.get("text", "") for b in content if isinstance(b, dict))
    return str(content)


async def main(ticket: str, amount: str) -> None:
    servers = json.loads((Path(__file__).parent / "mcp_servers.json").read_text())
    tools = await MultiServerMCPClient(servers).get_tools()
    agent = create_agent(ScriptedModel(ticket=ticket, amount=amount), tools,
                         system_prompt="You handle billing tickets.")
    result = await agent.ainvoke({"messages": [{"role": "user", "content": f"Ticket {ticket}: charge {amount}."}]})
    print(result["messages"][-1].content)


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1], sys.argv[2]))
