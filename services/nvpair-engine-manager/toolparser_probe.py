# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Find the vLLM tool-call parsers that read a model's tool-call format.

nvpair-engine-manager runs this with the Python of the vLLM it is about to
launch, passing the model's snapshot directory:

    python -I -c <this file> <snapshot dir>

A model's chat template writes an earlier tool call back into the conversation
in the same format the model uses when it calls a tool. So the template is
rendered with one example call, and that text is offered to every parser the
installed vLLM provides; a parser matches when it extracts exactly that call.

The last line printed is JSON: {"hf": bool, "matches": [parser names]}, where
hf means the tokenizer ships response_template metadata for vLLM's hf parser.
Nothing is downloaded, and no code from the model repository runs.
"""

import json
import os
import sys

os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

ARGS = {"city": "Paris"}
TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather in a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}
USER = {"role": "user", "content": "What is the weather in Paris?"}
RESULT = {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}


def rendered_call(tok):
    """Return the example call as the template writes it, or "" if it does not."""
    try:
        prompt = tok.apply_chat_template([USER], tools=[TOOL], add_generation_prompt=True, tokenize=False)
        base = tok.apply_chat_template([USER], tools=[TOOL], tokenize=False)
    except Exception:
        return ""
    # Templates differ on whether arguments are an object or a JSON string.
    for arguments in (ARGS, json.dumps(ARGS)):
        call = {
            "role": "assistant",
            "content": "",
            "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": arguments}}],
        }
        # Some templates render a final assistant turn as the generation prompt
        # and drop its calls; a tool result after it makes them write the call.
        for conversation in ([USER, call], [USER, call, RESULT]):
            try:
                full = tok.apply_chat_template(conversation, tools=[TOOL], tokenize=False)
            except Exception:
                continue
            for prefix in (prompt, base, os.path.commonprefix([prompt, full])):
                if full.startswith(prefix):
                    text = full[len(prefix):]
                    break
            if "get_weather" in text and "Paris" in text:
                return text
    return ""


def main():
    from transformers import AutoTokenizer

    tok = AutoTokenizer.from_pretrained(sys.argv[1], trust_remote_code=False)

    hf = False
    try:
        from vllm.parser.response_template import validate_tokenizer_response_template

        validate_tokenizer_response_template(tok, reasoning=False, tools=True)
        hf = True
    except Exception:
        pass

    matches = []
    text = rendered_call(tok)
    if text:
        try:
            from vllm.entrypoints.openai.chat_completion.protocol import ChatCompletionRequest
        except ImportError:
            from vllm.entrypoints.openai.protocol import ChatCompletionRequest
        from vllm.tool_parsers import ToolParserManager

        request = ChatCompletionRequest(model="probe", messages=[USER], tools=[TOOL])
        for name in sorted(ToolParserManager.list_registered()):
            if name == "hf":
                continue
            try:
                parser = ToolParserManager.get_tool_parser(name)(tok, tools=request.tools)
                result = parser.extract_tool_calls(text, request)
                calls = result.tool_calls if result.tools_called else []
                if (
                    len(calls) == 1
                    and calls[0].function.name == "get_weather"
                    and json.loads(calls[0].function.arguments) == ARGS
                ):
                    matches.append(name)
            except Exception:
                continue

    print(json.dumps({"hf": hf, "matches": matches}))


main()
