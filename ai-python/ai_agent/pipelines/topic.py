"""Topic tagging pipeline — assigns a short label to an active episode.

Called asynchronously by the Go backend after every message so the episode's
``current_topic`` field is populated and surfaced in the draft prompt.
"""

from __future__ import annotations

import logging

from ..llm import ModelRouter
from ..schemas import ModelTask, TopicTaggingRequest, TopicTaggingResponse
from ..security import PromptBuilder

logger = logging.getLogger(__name__)

_MAX_HISTORY = 5  # number of recent messages to include as context


def tag_topic(
    prompt_builder: PromptBuilder,
    router: ModelRouter,
    request: TopicTaggingRequest,
) -> TopicTaggingResponse:
    """Return a 3-7 word topic label for the active conversation episode.

    Uses a cheap LLM call on the extract model.  Returns an empty string on
    failure so the caller can skip the update without noise in logs.
    """
    if not request.message_text.strip():
        return TopicTaggingResponse(topic="")

    history = [m for m in request.recent_messages[-_MAX_HISTORY:] if m.strip()]
    if history:
        context_block = "\n".join(f"- {m}" for m in history)
        user_content = f"[RECENT MESSAGES]\n{context_block}\n\n[LATEST MESSAGE]\n{request.message_text}"
    else:
        user_content = request.message_text

    system_prompt, user_payload = prompt_builder.build(
        (
            "Label the main topic of this conversation in 3 to 7 words. "
            "The label should be a short noun phrase (e.g. 'обсуждение договора', "
            "'планирование встречи', 'вопрос по заказу'). "
            "Return ONLY the label — no punctuation, no explanation, no quotes."
        ),
        user_content,
        (),
    )
    model_used = ""
    try:
        response = router.complete(ModelTask.EXTRACT_FACTS, system_prompt, user_payload)
        model_used = response.model_used
        topic = response.text.strip().strip('"').strip("'")[:100]
        return TopicTaggingResponse(topic=topic, model_used=model_used)
    except Exception as exc:
        logger.warning("topic tagging failed", extra={"error": str(exc)})
        return TopicTaggingResponse(topic="", model_used=model_used)
