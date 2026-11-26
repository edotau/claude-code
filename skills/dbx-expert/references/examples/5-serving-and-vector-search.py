"""Model serving and vector search: https://databricks-sdk-py.readthedocs.io/en/latest/workspace/serving/index.html"""

from databricks.sdk import WorkspaceClient
from databricks.sdk.service.serving import ChatMessage, ChatMessageRole

w = WorkspaceClient(profile="dev")

# Serving endpoints: list them, then query a chat model.
for ep in w.serving_endpoints.list():
    print(ep.name, ep.state.ready if ep.state else None)
resp = w.serving_endpoints.query(
    name="databricks-claude-sonnet-5-5",
    messages=[ChatMessage(role=ChatMessageRole.USER, content="Say hello")],
    max_tokens=50,
)
print(resp.choices[0].message.content)

# The same endpoint through the OpenAI client (base URL and token come from the workspace client).
client = w.serving_endpoints.get_open_ai_client()
print(
    client.chat.completions.create(
        model="databricks-claude-sonnet-5-5",
        messages=[{"role": "user", "content": "Hi"}],
    )
)

# Vector search: query an index by text and print the hits.
hits = w.vector_search_indexes.query_index(
    index_name="main.default.docs_index",
    columns=["id", "text"],
    query_text="refund policy",
    num_results=5,
)
for row in hits.result.data_array:
    print(row)
